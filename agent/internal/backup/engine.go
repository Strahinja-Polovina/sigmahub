// Package backup implements the P1-11 typed backup ops: engine-native dumps
// piped into restic (client-side encrypted), restic check + GFS retention,
// automated restore-verify into a throwaway scratch container, and the
// fire-drill restore into a freshly provisioned database resource.
//
// Every command run inside a container is derived HERE from the engine name —
// the DSD op spec carries identifiers only, so a signed document cannot
// smuggle a shell command through the backup path (the no-generic-run-shell
// invariant holds).
package backup

import "fmt"

// dumpFilename is the stable in-snapshot filename for an engine's dump stream
// (restic --stdin-filename); verify addresses the same path via restic dump.
// baseBackupCommand streams a physical base backup (P2-5, postgres only) as a
// tar to stdout, fetching the WALs needed for consistency. This is the PITR
// starting point WAL segments replay from. Local unix-socket trust auth, like
// the dump path.
func baseBackupCommand(engine, username string) ([]string, error) {
	if engine != "postgres" {
		return nil, fmt.Errorf("base backup is postgres-only, got %q", engine)
	}
	return []string{"pg_basebackup", "-U", username, "-D", "-", "-Ft", "-X", "fetch"}, nil
}

// pitrRecoveryScript is the entrypoint for the throwaway PITR recovery
// container (P2-5b, postgres only). Running as root, it untars the base backup
// into PGDATA, stages the archived WAL, writes the recovery configuration
// (replay stops at recovery_target_time, then the cluster promotes), fixes
// ownership, and hands off to postgres as the postgres user. Like every other
// in-container command it is derived HERE from the engine — the DSD op carries
// only the target time, so no shell is smuggled through the op (the
// no-generic-run-shell invariant holds). targetTime is a validated RFC3339
// string (no quotes) so single-quoting it in the conf is injection-safe.
func pitrRecoveryScript(engine, targetTime string) ([]string, error) {
	if engine != "postgres" {
		return nil, fmt.Errorf("point-in-time recovery is postgres-only, got %q", engine)
	}
	script := `set -e
PGDATA=/var/lib/postgresql/data
WALDIR=/walrestore
mkdir -p "$PGDATA" "$WALDIR"
tar -xf /tmp/base.tar -C "$PGDATA"
for b in /tmp/wal-*.tar; do [ -e "$b" ] && tar -xf "$b" -C "$WALDIR"; done
touch "$PGDATA/recovery.signal"
{
  echo "restore_command = 'cp $WALDIR/%f %p'"
  echo "recovery_target_time = '` + targetTime + `'"
  echo "recovery_target_action = 'promote'"
  echo "archive_mode = 'off'"
} >> "$PGDATA/postgresql.auto.conf"
chown -R postgres:postgres "$PGDATA" "$WALDIR"
chmod 700 "$PGDATA"
exec gosu postgres postgres`
	return []string{"sh", "-c", script}, nil
}

func dumpFilename(engine string) string {
	switch engine {
	case "redis":
		return "dump.rdb"
	case "mongodb":
		return "dump.archive"
	default:
		return "dump.sql"
	}
}

// dumpCommand is the engine-native dump run INSIDE the database's own
// container via docker exec, writing the dump to stdout. Credentials come
// from the container's own environment (injected at create by P1-10) —
// resolved by the in-container shell, never carried in argv on the host side.
func dumpCommand(engine, database, username string) ([]string, error) {
	switch engine {
	case "postgres":
		// Local unix-socket auth: the postgres image trusts local connections.
		return []string{"pg_dump", "-U", username, database}, nil
	case "mysql":
		// MYSQL_PWD keeps the root password out of the process argv.
		return []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --databases ` + database}, nil
	case "redis":
		// REDISCLI_AUTH keeps the password out of argv; --rdb - streams the RDB.
		return []string{"sh", "-c", `REDISCLI_AUTH="$REDIS_PASSWORD" exec redis-cli --rdb -`}, nil
	case "mongodb":
		return []string{"sh", "-c", `exec mongodump --archive -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --quiet`}, nil
	}
	return nil, fmt.Errorf("backup: unsupported engine %q", engine)
}

// loadCommand loads a dump file (placed at path inside the container) into the
// engine — used for the verify scratch container and the fire-drill restore
// target. Redis has no SQL-load path: verify uses redis-check-rdb instead and
// restore-into-new-resource is unsupported in v1 (the CP rejects it early).
func loadCommand(engine, database, username, path string) ([]string, error) {
	switch engine {
	case "postgres":
		return []string{"psql", "-q", "-v", "ON_ERROR_STOP=1", "-U", username, "-d", database, "-f", path}, nil
	case "mysql":
		return []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -e "source ` + path + `"`}, nil
	case "redis":
		return []string{"redis-check-rdb", path}, nil
	case "mongodb":
		return []string{"sh", "-c", `exec mongorestore --archive=` + path + ` -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --quiet`}, nil
	}
	return nil, fmt.Errorf("backup: unsupported engine %q", engine)
}

// probeCommand asserts the engine answers queries after a load — the
// row-count/checksum probe's "does it actually serve" half (the byte-level
// half is the sha256 comparison against the recorded dump digest).
func probeCommand(engine, database, username string) ([]string, error) {
	switch engine {
	case "postgres":
		return []string{"psql", "-tA", "-U", username, "-d", database, "-c", "SELECT count(*) FROM information_schema.tables"}, nil
	case "mysql":
		return []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -N -e "SELECT COUNT(*) FROM information_schema.tables"`}, nil
	case "redis":
		// redis-check-rdb already validated the RDB; a running server is not
		// part of the redis verify contract.
		return nil, nil
	case "mongodb":
		return []string{"sh", "-c", `exec mongosh -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --quiet --eval "db.stats().ok"`}, nil
	}
	return nil, fmt.Errorf("backup: unsupported engine %q", engine)
}

// readyCommand polls the engine inside a scratch container until it accepts
// connections (bounded by the caller's deadline).
func readyCommand(engine, database, username string) ([]string, error) {
	switch engine {
	case "postgres":
		return []string{"pg_isready", "-U", username, "-d", database}, nil
	case "mysql":
		return []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqladmin -uroot ping`}, nil
	case "redis":
		return []string{"sh", "-c", `REDISCLI_AUTH="$REDIS_PASSWORD" exec redis-cli ping`}, nil
	case "mongodb":
		return []string{"sh", "-c", `exec mongosh -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --quiet --eval "db.runCommand({ping:1}).ok"`}, nil
	}
	return nil, fmt.Errorf("backup: unsupported engine %q", engine)
}

// scratchEnv is the bootstrap environment for a throwaway verify container.
// The password is a fixed throwaway — the container joins no network, publishes
// no port, holds only data restored from the org's own snapshot, and is
// removed when verify completes.
func scratchEnv(engine, database, username string) map[string]string {
	const throwaway = "sigmahub-verify"
	switch engine {
	case "postgres":
		return map[string]string{"POSTGRES_USER": username, "POSTGRES_DB": database, "POSTGRES_PASSWORD": throwaway}
	case "mysql":
		return map[string]string{"MYSQL_DATABASE": database, "MYSQL_USER": username, "MYSQL_PASSWORD": throwaway, "MYSQL_ROOT_PASSWORD": throwaway}
	case "redis":
		return map[string]string{"REDIS_PASSWORD": throwaway}
	case "mongodb":
		return map[string]string{"MONGO_INITDB_ROOT_USERNAME": username, "MONGO_INITDB_ROOT_PASSWORD": throwaway}
	}
	return nil
}
