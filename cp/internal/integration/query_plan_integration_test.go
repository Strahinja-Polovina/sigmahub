// Query-plan regression tests.
//
// Some of this schema's indexes exist to keep a hot background loop's cost
// proportional to the WORK OUTSTANDING rather than to the install's accumulated
// history. That property is invisible in a functional test: a query that
// sequentially scans 200,000 rows returns exactly the same answer as one that
// probes a partial index, just 10,000 times more expensively. It is also easy
// to lose by accident — SIGMA-332 lost it by adding a third OR arm to a WHERE
// clause — and nothing fails when it goes.
//
// So these tests load enough rows for the planner to have a real choice, ANALYZE
// so it has statistics to choose with, and assert on the plan itself. They are
// deliberately about the SHAPE of the plan (index vs sequential scan on the
// growing table), never about timings, so they do not flake on a loaded box.
package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// explainJSON returns the plan for a query as a single flattened string, so a
// test can assert on node types and index names without walking the JSON tree.
// EXPLAIN without ANALYZE only plans — nothing is executed, no rows are locked
// even for a FOR UPDATE query.
func explainJSON(t *testing.T, st *store.Store, query string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	var plan []byte
	if err := st.Pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+query, args...).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var pretty any
	if err := json.Unmarshal(plan, &pretty); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	out, _ := json.Marshal(pretty)
	return string(out)
}

// TestDeployDrainQueryUsesIndex is SIGMA-317. runDeployDrain runs the drain
// query every 3 seconds — 28,800 times a day — and on a healthy install it
// almost always finds nothing, because deploy_requests rows go 'queued' →
// 'drained' within one tick and then stay forever. With only
// deploy_requests_org_idx (org_id, created_at) on the table, no index can drive
// a predicate on (kind, status), so each of those 28,800 passes sequentially
// scans the ENTIRE push history and takes row locks under FOR UPDATE SKIP
// LOCKED to find zero work. The cost grows monotonically with the install's
// age and never shrinks.
func TestDeployDrainQueryUsesIndex(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	proj, err := st.CreateProject(ctx, "org_plan", "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO git_connections (id, org_id, project_id, provider, installation_id, repo_full_name, created_by)
		VALUES ('gc_plan', 'org_plan', $1, 'github', 'inst', 'acme/app', 'test')`, proj.ID); err != nil {
		t.Fatal(err)
	}
	// A year of pushes, all long since drained: the steady state of any install
	// that has been running a while.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO deploy_requests (id, org_id, connection_id, kind, ref, sha, status, created_at)
		SELECT 'dr_' || i, 'org_plan', 'gc_plan', 'deploy', 'refs/heads/main', md5(i::text), 'drained',
		       now() - (i || ' minutes')::interval
		  FROM generate_series(1, 50000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `ANALYZE deploy_requests`); err != nil {
		t.Fatal(err)
	}

	plan := explainJSON(t, st, store.DrainDeployRequestsQuery)
	if planHasSeqScanOn(plan, "deploy_requests") {
		t.Fatalf("drain query sequentially scans deploy_requests; plan = %s", plan)
	}
	if !strings.Contains(plan, "deploy_requests_queued_idx") {
		t.Fatalf("drain query does not use deploy_requests_queued_idx; plan = %s", plan)
	}
}

// planHasSeqScanOn reports whether the plan contains a Seq Scan node over the
// named relation. The JSON is flattened, so this looks for the node type and the
// relation name inside the same object by scanning the serialised form: every
// plan node serialises "Node Type" before "Relation Name", with nothing but that
// node's own fields between them.
func planHasSeqScanOn(plan, relation string) bool {
	for _, chunk := range strings.Split(plan, `"Node Type":"Seq Scan"`)[1:] {
		// Stop at the next node boundary so we only look at this node's fields.
		if end := strings.Index(chunk, `"Node Type"`); end >= 0 {
			chunk = chunk[:end]
		}
		if strings.Contains(chunk, `"Relation Name":"`+relation+`"`) {
			return true
		}
	}
	return false
}
