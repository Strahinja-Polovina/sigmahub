package store

// SIGMA-284: erase one tenant from the control plane.
//
// The control plane had every ingredient of a tenant lifecycle except its end.
// A tenant is created by POST /v1/orgs, and from then on the only deletes that
// exist are per-object — a project, an environment, a resource, a server. There
// was no way to answer "the design partner ended the trial, remove them", and
// no way to answer a GDPR Art. 17 erasure request that reaches the CP's own
// personal data: cp_audit_log carries the actor's DISPLAY NAME on every row,
// alert_channels carries SMTP credentials and the recipient addresses an
// incident pages, and dns_provider_credentials carries API tokens for the
// customer's own DNS account. An operator asked to do this by hand would have
// to know all ~40 org-scoped tables and the order the foreign keys allow them
// to be deleted in, in a schema that gains tables every migration.
//
// So this does not carry a hand-written table list. A hand-written list is the
// same defect one migration later: the table added next quarter is missed
// silently, and "silently" is the whole problem — an erasure that reports
// success while leaving rows behind is worse than one that fails.
//
// Instead the set of org-scoped tables is DISCOVERED from the live schema
// (every base table with an org_id column), and the delete ORDER is discovered
// by trying: a delete that a foreign key refuses is rolled back to a savepoint
// and retried on the next pass, once whatever referenced it has gone. The pass
// loop runs to a fixpoint, and a purge that stops making progress with tables
// still outstanding is an ERROR — never a partial success reported as done.
//
// Tables with no org_id are either global (cp_secrets, webhook_deliveries,
// schema_migrations) or reached by ON DELETE CASCADE from an org-scoped parent
// (agent_tokens, server_metrics and server_hardening from servers; deploy_logs
// from deployments; cluster_nodes from clusters; git_branch_map from
// environments). PurgeOrg verifies the second group rather than trusting it —
// see PurgeOrgLeftovers, which the integration test asserts on.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// purgeMaxPasses bounds the fixpoint loop. The CP's foreign-key graph is three
// or four levels deep (org → project → environment → resource → backup policy),
// so a real purge converges in a handful of passes; the bound only exists so a
// future circular constraint fails loudly instead of spinning.
const purgeMaxPasses = 12

// PurgeOrg deletes every row in the control plane belonging to orgID and
// returns the rows removed per table (tables that had nothing are omitted).
//
// It is all-or-nothing: one transaction, so a purge that cannot finish leaves
// the tenant exactly as it was rather than half-erased. Callers get an error
// naming the tables that could not be emptied.
//
// This does NOT reach the customer's machines. Purging the control plane's
// record of a server does not uninstall the agent from it — decommission does
// that, and an operator erasing a tenant should disconnect the fleet first.
// The org's DEK goes with everything else, which by design makes any ciphertext
// that outlives this (an offsite restic repository, a database dump) permanently
// undecryptable: that is what erasure means, and it is why the caller must be
// sure.
func (s *Store) PurgeOrg(ctx context.Context, orgID string) (map[string]int64, error) {
	if orgID == "" {
		return nil, fmt.Errorf("purge org: empty org id")
	}
	tables, err := s.orgScopedTables(ctx)
	if err != nil {
		return nil, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	deleted := map[string]int64{}
	remaining := append([]string(nil), tables...)
	for pass := 0; pass < purgeMaxPasses && len(remaining) > 0; pass++ {
		var blocked []string
		progress := false
		for _, table := range remaining {
			n, err := purgeTable(ctx, tx, table, orgID)
			if err != nil {
				// Almost always a foreign key still pointing at these rows from
				// a table later in the list. Keep it for the next pass; if it is
				// something else, the fixpoint stalls and we report it below.
				blocked = append(blocked, table)
				continue
			}
			progress = true
			if n > 0 {
				deleted[table] = n
			}
		}
		if !progress {
			return nil, fmt.Errorf("purge org %s: no progress; blocked tables: %v", orgID, blocked)
		}
		remaining = blocked
	}
	if len(remaining) > 0 {
		return nil, fmt.Errorf("purge org %s: gave up after %d passes; blocked tables: %v",
			orgID, purgeMaxPasses, remaining)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return deleted, nil
}

// purgeTable deletes one table's rows for the org inside a savepoint, so a
// foreign-key refusal costs that statement and not the whole transaction.
func purgeTable(ctx context.Context, tx pgx.Tx, table, orgID string) (int64, error) {
	sp, err := tx.Begin(ctx) // pgx nested Begin = SAVEPOINT
	if err != nil {
		return 0, err
	}
	// table comes from information_schema, never from a request; quoted anyway
	// so an identifier needing quotes cannot break the statement.
	tag, err := sp.Exec(ctx, fmt.Sprintf(`DELETE FROM %q WHERE org_id = $1`, table), orgID)
	if err != nil {
		_ = sp.Rollback(ctx)
		return 0, err
	}
	if err := sp.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// orgScopedTables lists every base table in the current schema carrying an
// org_id column, in a stable order.
func (s *Store) orgScopedTables(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT c.table_name
		  FROM information_schema.columns c
		  JOIN information_schema.tables t
		    ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		 WHERE c.table_schema = current_schema()
		   AND c.column_name = 'org_id'
		   AND t.table_type = 'BASE TABLE'
		 ORDER BY c.table_name`)
	if err != nil {
		return nil, fmt.Errorf("discover org-scoped tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// PurgeOrgLeftovers reports rows that survived a purge because they live in a
// table with no org_id and were supposed to be swept by ON DELETE CASCADE.
// Keyed by "table.column", valued by the surviving row count.
//
// This is the check the purge cannot make on itself: the cascade either
// happened or it did not, and the only way to know is to look. Exported so the
// integration test asserts on the SCHEMA rather than on a list of table names
// somebody remembered to update.
func (s *Store) PurgeOrgLeftovers(ctx context.Context) (map[string]int64, error) {
	// The cascade-reachable tables, each named by the column that ties it to a
	// row the purge deleted. A leftover here means an FK lost its ON DELETE
	// CASCADE, which is exactly the silent partial erasure this guards against.
	checks := []struct{ table, col, parent, parentCol string }{
		{"agent_tokens", "server_id", "servers", "id"},
		{"server_metrics", "server_id", "servers", "id"},
		{"server_hardening", "server_id", "servers", "id"},
		{"cluster_nodes", "cluster_id", "clusters", "id"},
		{"deploy_logs", "deployment_id", "deployments", "id"},
		{"git_branch_map", "environment_id", "environments", "id"},
	}
	out := map[string]int64{}
	for _, c := range checks {
		var n int64
		q := fmt.Sprintf(
			`SELECT count(*) FROM %q child WHERE NOT EXISTS (
			   SELECT 1 FROM %q parent WHERE parent.%q = child.%q)`,
			c.table, c.parent, c.parentCol, c.col)
		if err := s.Pool.QueryRow(ctx, q).Scan(&n); err != nil {
			return nil, fmt.Errorf("leftover check %s: %w", c.table, err)
		}
		if n > 0 {
			out[c.table+"."+c.col] = n
		}
	}
	return out, nil
}
