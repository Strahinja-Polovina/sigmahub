package store

import (
	"strconv"
	"strings"
)

// DBEngineDef is the single source of truth for one supported database engine:
// its pinned image, container port, data mount, credential env-var contract,
// connection-URL shape and server-type tuning profile. The reconciler renders
// from it, the credential resolve path injects from it, and the dashboard's
// generated catalog is rendered from it, so no side can drift from another.
//
// Tuning is deliberately container-level knobs only (engine config args) —
// host-level tuning (IO scheduler, dedicated volumes) is out of the typed op
// vocabulary and explicitly deferred.
type DBEngineDef struct {
	Engine        string // canonical name == resource kind
	Image         string // pinned version tag; the agent policy refuses floating tags
	ContainerPort int
	DataMount     string // where the named "data" volume mounts
	// URLTemplate is the connection string's SHAPE: {username}, {password},
	// {host}, {port} and {database}, each substituted once (ConnectionURL).
	//
	// A template rather than a switch statement because the dashboard has to
	// render the same string in demo mode, where there is no control plane to
	// ask. Demo mode kept its own switch, and that copy appended
	// ?sslmode=disable to the Postgres URL and put the database in the MongoDB
	// path — two connection strings this product never hands out, printed under
	// a panel headed "your connection details".
	URLTemplate string
	// SecretEnvNames are the env vars carrying generated credentials. They are
	// rendered into the DSD as secret REFERENCES and resolved agent-side at
	// container create, so a captured DSD leaks nothing.
	SecretEnvNames []string
}

// MeshPortBase is the first mesh-bound host port the allocator hands out, and
// therefore the first number a connection panel can print. The range is
// per-server (a unique index on (server_id, port) is the backstop) and never
// collides with the engines' well-known container ports — which is the whole
// point of it: a managed engine answers on an ALLOCATED mesh port, never on
// 5432, and every panel that says otherwise is teaching a port that will not
// connect.
//
// It lives beside the engine catalog rather than beside allocateDBPort because
// it is RENDERED into the dashboard (server_catalog_ts.go). Every file that
// reaches the generated output is hashed into the catalog digest, and hashing
// databases.go would make each unrelated query edit there demand a regenerate
// that changes no byte.
const MeshPortBase = 15000

// DB engine catalog (P1-10). Version pins are deliberate: a floating tag would
// let a database change out from under the pinned DSD (and the agent policy
// refuses it anyway). ClickHouse and friends are explicit non-goals.
var dbEngines = map[string]DBEngineDef{
	"postgres": {
		Engine: "postgres", Image: "postgres:16.6", ContainerPort: 5432,
		DataMount:      "/var/lib/postgresql/data",
		URLTemplate:    "postgresql://{username}:{password}@{host}:{port}/{database}",
		SecretEnvNames: []string{"POSTGRES_PASSWORD"},
	},
	"mysql": {
		Engine: "mysql", Image: "mysql:8.4.4", ContainerPort: 3306,
		DataMount:      "/var/lib/mysql",
		URLTemplate:    "mysql://{username}:{password}@{host}:{port}/{database}",
		SecretEnvNames: []string{"MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD"},
	},
	"redis": {
		Engine: "redis", Image: "redis:7.4.2", ContainerPort: 6379,
		DataMount: "/data",
		// No username and no database name: Redis authenticates with the
		// password alone, and a URL that invented a database would not connect.
		URLTemplate:    "redis://:{password}@{host}:{port}/0",
		SecretEnvNames: []string{"REDIS_PASSWORD"},
	},
	"mongodb": {
		Engine: "mongodb", Image: "mongo:7.0.16", ContainerPort: 27017,
		DataMount: "/data/db",
		// The database is deliberately absent from the path: credentials are
		// created on the admin database, so authSource=admin is what makes the
		// URL authenticate at all.
		URLTemplate:    "mongodb://{username}:{password}@{host}:{port}/?authSource=admin",
		SecretEnvNames: []string{"MONGO_INITDB_ROOT_PASSWORD"},
	},
}

// The catalog is rendered into a checked-in file and read by name everywhere
// else, so the ways one entry can be quietly wrong ON ITS OWN TERMS are worth
// failing at package load rather than at the first CreateResource of the week:
// an engine that calls itself something other than its key resolves to a
// definition describing a different database, and an engine with no image or no
// URL template renders an empty connection string on both sides.
//
// The other half of being right — that every key here is a resource kind in the
// catalog's database category, and that every such kind has an entry here — is
// checked in server_catalog.go's init instead of this one, with a comment there
// naming the defect each direction catches (SIGMA-216). That is not a
// preference: package init functions run in file-name order, db_engines.go sorts
// before server_catalog.go, and the category transpose those checks read does
// not exist yet when this function runs.
func init() {
	for kind, def := range dbEngines {
		if def.Engine != kind {
			panic("store: db engine keyed " + kind + " calls itself " + def.Engine)
		}
		if def.URLTemplate == "" {
			panic("store: db engine " + kind + " has no connection-URL template")
		}
		if def.Image == "" {
			panic("store: db engine " + kind + " has no image")
		}
	}
}

// DBEngine returns the engine definition for a resource kind (ok=false for
// non-database kinds).
func DBEngine(kind string) (DBEngineDef, bool) {
	def, ok := dbEngines[kind]
	return def, ok
}

// IsDBKind reports whether a resource kind is a database engine.
func IsDBKind(kind string) bool { _, ok := dbEngines[kind]; return ok }

// DBEngineCatalog returns every engine definition in resource-kind order.
//
// The order is the catalog's, not the map's: this is what the TypeScript
// generator renders, the rendered file is checked in, and a map range would
// reshuffle it on most runs — making `go generate` produce a different file
// each time and failing the staleness test on commits that changed nothing.
func DBEngineCatalog() []DBEngineDef {
	out := make([]DBEngineDef, 0, len(dbEngines))
	for _, kind := range ResourceKinds() {
		if def, ok := dbEngines[kind]; ok {
			out = append(out, def)
		}
	}
	return out
}

// DBEngineKinds lists the database kinds in the same order.
//
// It is also the answer to "which engines may an operator enable" — config.go
// validates CP_DB_ENGINES against this rather than keeping a fourth spelling of
// the same four names. That is sound rather than convenient: server_catalog.go's
// init refuses to load a package in which this list and the catalog's database
// category differ, so "engines with a definition" and "kinds the wizard calls a
// database" cannot come apart behind the allowlist's back.
func DBEngineKinds() []string {
	out := make([]string, 0, len(dbEngines))
	for _, def := range DBEngineCatalog() {
		out = append(out, def.Engine)
	}
	return out
}

// PlainEnv is the engine's NON-secret environment (usernames and database
// names are identifiers, not secrets — the password never appears here).
//
// Redis is a named case rather than the default arm, which is the difference
// between "this engine has no user or database concept" and "we have not
// thought about this engine". The default arm used to carry a comment saying it
// belonged to redis, so an engine added to dbEngines silently inherited Redis's
// semantics — a container started with no credentials env at all, while the
// connection panel printed a URL naming a user the engine was never told to
// create. Falling through now returns nil under a name nobody claimed, and
// TestEveryEngineIsStartedWithCredentialsAndACommand refuses it.
func (d DBEngineDef) PlainEnv(username, dbname string) map[string]string {
	switch d.Engine {
	case "postgres":
		return map[string]string{"POSTGRES_USER": username, "POSTGRES_DB": dbname}
	case "mysql":
		return map[string]string{"MYSQL_USER": username, "MYSQL_DATABASE": dbname}
	case "mongodb":
		return map[string]string{"MONGO_INITDB_ROOT_USERNAME": username}
	case "redis":
		// No user or database concept: the password is the whole credential and
		// it is injected as a secret reference, never here.
		return nil
	default:
		return nil
	}
}

// TunedCommand is the engine start command for a server-type tuning profile:
// database-type servers get production-grade settings, everything else the
// conservative dev-grade defaults. Redis takes its password from the injected
// env var via the shell so the DSD never carries it (args are spec, env refs
// are resolved agent-side).
func (d DBEngineDef) TunedCommand(serverType string) []string {
	prod := serverType == "database"
	switch d.Engine {
	case "postgres":
		if prod {
			return []string{"postgres", "-c", "shared_buffers=512MB", "-c", "max_connections=200"}
		}
		return []string{"postgres", "-c", "shared_buffers=128MB", "-c", "max_connections=50"}
	case "mysql":
		if prod {
			return []string{"mysqld", "--innodb-buffer-pool-size=512M"}
		}
		return []string{"mysqld", "--innodb-buffer-pool-size=128M"}
	case "redis":
		maxmem := "128mb"
		if prod {
			maxmem = "512mb"
		}
		return []string{"sh", "-c",
			`exec redis-server --appendonly yes --maxmemory ` + maxmem + ` --maxmemory-policy noeviction --requirepass "$REDIS_PASSWORD"`}
	case "mongodb":
		if prod {
			return []string{"mongod", "--wiredTigerCacheSizeGB", "0.50"}
		}
		return []string{"mongod", "--wiredTigerCacheSizeGB", "0.25"}
	}
	return nil
}

// ConnectionURL renders the engine's canonical connection string for the
// mesh-internal address — port is the ALLOCATED mesh port, never the container
// port. Password is the caller's responsibility (audited reveal only). An
// engine this catalog does not know renders nothing rather than a URL shaped
// like a guess.
func (d DBEngineDef) ConnectionURL(username, password, host string, port int, dbname string) string {
	if d.URLTemplate == "" {
		return ""
	}
	return strings.NewReplacer(
		"{username}", username,
		"{password}", password,
		"{host}", host,
		"{port}", strconv.Itoa(port),
		"{database}", dbname,
	).Replace(d.URLTemplate)
}
