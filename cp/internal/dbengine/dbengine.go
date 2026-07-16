// Package dbengine is the P1-10 engine-pluggable database provisioner: each
// supported engine (PostgreSQL, MySQL, Redis/Valkey, MongoDB) describes how its
// pinned container runs — image, port, data volume, credential injection, and
// per-server-type tuning — and the reconciler renders those descriptions into the
// P1-3 typed container ops. Cutting the engine set (the pre-agreed M6 fallback:
// Postgres-only) is a configuration change, not a rewrite: engines register here
// and CP_DB_ENGINES selects the enabled subset.
package dbengine

import (
	"fmt"
	"sort"
	"strings"
)

// Profile selects the engine's container-level tuning knobs by server type:
// prod-grade settings on database-type servers, dev-grade defaults elsewhere.
// Host-level tuning (IO scheduler, dedicated volumes) is deliberately out of
// scope — it is not expressible in the typed op vocabulary.
type Profile string

const (
	ProfileDatabase Profile = "database"
	ProfileGeneral  Profile = "general"
)

// ProfileForServerType maps a server's type to a tuning profile.
func ProfileForServerType(serverType string) Profile {
	if serverType == "database" {
		return ProfileDatabase
	}
	return ProfileGeneral
}

// Credentials are the generated per-database credentials. Password is the only
// secret: it is envelope-encrypted at rest (P1-6 DEK) and injected into the
// container by the agent as a reference, never rendered into the DSD. Username
// and database name are not secret and ride in plain container env.
type Credentials struct {
	Username string
	Password string
	Database string
}

// SecretInjection describes how the engine receives its password: as an env var
// (name = EnvName) or as a file seeded into the tmpfs secrets mount (name =
// FileName, content = FileContent(password)).
type SecretInjection struct {
	// EnvName is the env var carrying the password ("" for file-mode engines).
	EnvName string
	// FileName is the tmpfs secrets file name ("" for env-mode engines).
	FileName string
	// FileContent renders the seeded file from the password (file mode only).
	FileContent func(password string) string
}

// Engine describes one database engine's containerisation.
type Engine struct {
	// Kind matches the resource kind ("postgres" | "mysql" | "redis" | "mongodb").
	Kind string
	// Image is the pinned container image.
	Image string
	// Port is the engine's listener port inside the container.
	Port int
	// DataPath is the mount path of the named data volume.
	DataPath string
	// Env renders the non-secret container environment (username/database and
	// any static toggles). The password is injected separately via Secret.
	Env func(c Credentials) map[string]string
	// Secret describes the password injection.
	Secret SecretInjection
	// Command optionally overrides the container command (e.g. Redis waits for
	// its seeded conf). The command must never embed a credential.
	Command []string
	// Tuning returns extra container args (appended to Command, or used as the
	// command for engines whose entrypoint accepts flags) per profile.
	Tuning func(p Profile) []string
	// ConnString renders the client connection string with the plaintext
	// password; host is the mesh address (mesh-internal only in v1).
	ConnString func(c Credentials, host string, port int) string
}

// registry holds every compiled-in engine.
var registry = map[string]Engine{}

func register(e Engine) { registry[e.Kind] = e }

// Enabled returns the enabled engine kinds given the CP_DB_ENGINES config value
// (comma-separated; empty enables every compiled-in engine). Unknown names are
// ignored. Sorted for deterministic iteration.
func Enabled(config string) []string {
	var kinds []string
	if strings.TrimSpace(config) == "" {
		for k := range registry {
			kinds = append(kinds, k)
		}
	} else {
		for _, k := range strings.Split(config, ",") {
			k = strings.TrimSpace(strings.ToLower(k))
			if _, ok := registry[k]; ok {
				kinds = append(kinds, k)
			}
		}
	}
	sort.Strings(kinds)
	return kinds
}

// Get returns the engine for a kind (ok=false for non-DB kinds or engines not
// compiled in).
func Get(kind string) (Engine, bool) {
	e, ok := registry[kind]
	return e, ok
}

// IsDBKind reports whether a resource kind is a database engine.
func IsDBKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// DerivedIdentity is the deterministic (non-secret) username + database name for
// a resource — derived, not stored, so the reconciler renders container env
// without a credentials query and the store provisions the matching row.
func DerivedIdentity(resourceID string) (username, database string) {
	suffix := resourceID
	if i := len(suffix) - 8; i > 0 {
		suffix = suffix[i:]
	}
	return "app_" + suffix, "app"
}

func init() {
	register(Engine{
		Kind:     "postgres",
		Image:    "postgres:16.4",
		Port:     5432,
		DataPath: "/var/lib/postgresql/data",
		Env: func(c Credentials) map[string]string {
			return map[string]string{"POSTGRES_USER": c.Username, "POSTGRES_DB": c.Database}
		},
		Secret: SecretInjection{EnvName: "POSTGRES_PASSWORD"},
		Tuning: func(p Profile) []string {
			if p == ProfileDatabase {
				// Prod-grade: sized for a dedicated database host; observable via
				// SHOW shared_buffers (the acceptance inspection probe).
				return []string{"-c", "shared_buffers=1GB", "-c", "max_connections=200", "-c", "effective_cache_size=3GB"}
			}
			return nil // image defaults (128MB shared_buffers) — dev grade
		},
		ConnString: func(c Credentials, host string, port int) string {
			return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s", c.Username, c.Password, host, port, c.Database)
		},
	})

	register(Engine{
		Kind:     "mysql",
		Image:    "mysql:8.4",
		Port:     3306,
		DataPath: "/var/lib/mysql",
		Env: func(c Credentials) map[string]string {
			return map[string]string{
				"MYSQL_USER":                 c.Username,
				"MYSQL_DATABASE":             c.Database,
				"MYSQL_RANDOM_ROOT_PASSWORD": "yes", // root stays unreachable; the generated user is the credential
			}
		},
		Secret: SecretInjection{EnvName: "MYSQL_PASSWORD"},
		Tuning: func(p Profile) []string {
			if p == ProfileDatabase {
				return []string{"--innodb-buffer-pool-size=1G", "--max-connections=200"}
			}
			return nil
		},
		ConnString: func(c Credentials, host string, port int) string {
			return fmt.Sprintf("mysql://%s:%s@%s:%d/%s", c.Username, c.Password, host, port, c.Database)
		},
	})

	register(Engine{
		Kind:     "redis",
		Image:    "redis:7.4",
		Port:     6379,
		DataPath: "/data",
		Env:      func(Credentials) map[string]string { return nil },
		// The official Redis image has no env-based auth, and putting the password
		// in the command would render it into the (signed, journaled) DSD. Instead
		// the password rides the P1-6 FILE channel: the agent seeds
		// /run/secrets/REDIS_CONF into the container tmpfs right after start, and
		// the command waits for it, then execs redis-server on it — the credential
		// never appears in the DSD, argv, or any on-disk layer.
		Secret: SecretInjection{
			FileName: "REDIS_CONF",
			FileContent: func(password string) string {
				return "requirepass " + password + "\nprotected-mode yes\n"
			},
		},
		Command: []string{"sh", "-c", "while [ ! -f /run/secrets/REDIS_CONF ]; do sleep 0.2; done; exec redis-server /run/secrets/REDIS_CONF \"$@\"", "--"},
		Tuning: func(p Profile) []string {
			if p == ProfileDatabase {
				return []string{"--maxmemory", "1gb", "--maxmemory-policy", "allkeys-lru"}
			}
			return nil
		},
		ConnString: func(c Credentials, host string, port int) string {
			return fmt.Sprintf("redis://:%s@%s:%d/0", c.Password, host, port)
		},
	})

	register(Engine{
		Kind:     "mongodb",
		Image:    "mongo:7.0",
		Port:     27017,
		DataPath: "/data/db",
		Env: func(c Credentials) map[string]string {
			return map[string]string{"MONGO_INITDB_ROOT_USERNAME": c.Username, "MONGO_INITDB_DATABASE": c.Database}
		},
		Secret: SecretInjection{EnvName: "MONGO_INITDB_ROOT_PASSWORD"},
		Tuning: func(p Profile) []string {
			if p == ProfileDatabase {
				return []string{"--wiredTigerCacheSizeGB", "1"}
			}
			return nil
		},
		ConnString: func(c Credentials, host string, port int) string {
			return fmt.Sprintf("mongodb://%s:%s@%s:%d/%s?authSource=admin", c.Username, c.Password, host, port, c.Database)
		},
	})
}
