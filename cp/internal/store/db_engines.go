package store

import "fmt"

// DBEngineDef is the single source of truth for one supported database engine:
// its pinned image, container port, data mount, credential env-var contract and
// server-type tuning profile. The reconciler renders from it and the credential
// resolve path injects from it, so the two sides cannot drift.
//
// Tuning is deliberately container-level knobs only (engine config args) —
// host-level tuning (IO scheduler, dedicated volumes) is out of the typed op
// vocabulary and explicitly deferred.
type DBEngineDef struct {
	Engine        string // canonical name == resource kind
	Image         string // pinned version tag; the agent policy refuses floating tags
	ContainerPort int
	DataMount     string // where the named "data" volume mounts
	// SecretEnvNames are the env vars carrying generated credentials. They are
	// rendered into the DSD as secret REFERENCES and resolved agent-side at
	// container create, so a captured DSD leaks nothing.
	SecretEnvNames []string
}

// DB engine catalog (P1-10). Version pins are deliberate: a floating tag would
// let a database change out from under the pinned DSD (and the agent policy
// refuses it anyway). ClickHouse and friends are explicit non-goals.
var dbEngines = map[string]DBEngineDef{
	"postgres": {
		Engine: "postgres", Image: "postgres:16.6", ContainerPort: 5432,
		DataMount:      "/var/lib/postgresql/data",
		SecretEnvNames: []string{"POSTGRES_PASSWORD"},
	},
	"mysql": {
		Engine: "mysql", Image: "mysql:8.4.4", ContainerPort: 3306,
		DataMount:      "/var/lib/mysql",
		SecretEnvNames: []string{"MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD"},
	},
	"redis": {
		Engine: "redis", Image: "redis:7.4.2", ContainerPort: 6379,
		DataMount:      "/data",
		SecretEnvNames: []string{"REDIS_PASSWORD"},
	},
	"mongodb": {
		Engine: "mongodb", Image: "mongo:7.0.16", ContainerPort: 27017,
		DataMount:      "/data/db",
		SecretEnvNames: []string{"MONGO_INITDB_ROOT_PASSWORD"},
	},
}

// DBEngine returns the engine definition for a resource kind (ok=false for
// non-database kinds).
func DBEngine(kind string) (DBEngineDef, bool) {
	def, ok := dbEngines[kind]
	return def, ok
}

// IsDBKind reports whether a resource kind is a database engine.
func IsDBKind(kind string) bool { _, ok := dbEngines[kind]; return ok }

// PlainEnv is the engine's NON-secret environment (usernames and database
// names are identifiers, not secrets — the password never appears here).
func (d DBEngineDef) PlainEnv(username, dbname string) map[string]string {
	switch d.Engine {
	case "postgres":
		return map[string]string{"POSTGRES_USER": username, "POSTGRES_DB": dbname}
	case "mysql":
		return map[string]string{"MYSQL_USER": username, "MYSQL_DATABASE": dbname}
	case "mongodb":
		return map[string]string{"MONGO_INITDB_ROOT_USERNAME": username}
	default: // redis has no user/db concept
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
// mesh-internal address. Password is the caller's responsibility (audited
// reveal only).
func (d DBEngineDef) ConnectionURL(username, password, host string, port int, dbname string) string {
	switch d.Engine {
	case "postgres":
		return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s", username, password, host, port, dbname)
	case "mysql":
		return fmt.Sprintf("mysql://%s:%s@%s:%d/%s", username, password, host, port, dbname)
	case "redis":
		return fmt.Sprintf("redis://:%s@%s:%d/0", password, host, port)
	case "mongodb":
		return fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin", username, password, host, port)
	}
	return ""
}
