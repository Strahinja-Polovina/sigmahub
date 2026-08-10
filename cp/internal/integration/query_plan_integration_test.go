// Query-plan regression tests.
//
// Some of this schema's indexes exist to keep a hot loop's cost proportional to
// the WORK OUTSTANDING rather than to the install's accumulated history. That
// property is invisible in a functional test: a query that sequentially scans
// 200,000 rows returns exactly the same answer as one that probes a partial
// index, just 10,000 times more expensively. It is also easy to lose by
// accident — SIGMA-332 lost it by adding a third OR arm to a WHERE clause — and
// nothing fails when it goes.
//
// So these tests load enough rows for the planner to have a real choice, ANALYZE
// so it has statistics to choose with, and assert on the plan itself: which
// relations are scanned sequentially, which index drives the scan, and how many
// rows the scan actually touched. They never assert on TIMINGS, so they do not
// flake on a loaded box.
package integration

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// planNode is the subset of EXPLAIN (FORMAT JSON) these tests assert on.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	ActualRows   float64    `json:"Actual Rows"`
	ActualLoops  float64    `json:"Actual Loops"`
	Plans        []planNode `json:"Plans"`
}

func (n planNode) walk(f func(planNode)) {
	f(n)
	for _, c := range n.Plans {
		c.walk(f)
	}
}

// seqScanOn reports whether the plan reads the named relation sequentially.
func (n planNode) seqScanOn(relation string) bool {
	found := false
	n.walk(func(p planNode) {
		if p.NodeType == "Seq Scan" && p.RelationName == relation {
			found = true
		}
	})
	return found
}

func (n planNode) usesIndex(name string) bool {
	found := false
	n.walk(func(p planNode) {
		if p.IndexName == name {
			found = true
		}
	})
	return found
}

// rowsTouched totals the rows every scan of a relation actually produced,
// across all loops of a nested loop. This is the number the cost tickets are
// about: a plan that touches 40,000 deployment rows to answer a question about
// 300 resources is the defect, whatever index it happens to use on the way.
// Only meaningful for plans produced with ANALYZE.
func (n planNode) rowsTouched(relation string) float64 {
	var total float64
	n.walk(func(p planNode) {
		if p.RelationName != relation {
			return
		}
		loops := p.ActualLoops
		if loops == 0 {
			loops = 1
		}
		total += p.ActualRows * loops
	})
	return total
}

// explainPlan returns the root plan node. analyze=false plans without executing,
// which is what a FOR UPDATE query needs — EXPLAIN alone takes no row locks.
func explainPlan(t *testing.T, st *store.Store, analyze bool, query string, args ...any) planNode {
	t.Helper()
	prefix := "EXPLAIN (FORMAT JSON) "
	if analyze {
		prefix = "EXPLAIN (ANALYZE, FORMAT JSON) "
	}
	var raw []byte
	if err := st.Pool.QueryRow(context.Background(), prefix+query, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var wrapper []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if len(wrapper) == 0 {
		t.Fatalf("empty plan for %s", query)
	}
	return wrapper[0].Plan
}

func planJSON(t *testing.T, n planNode) string {
	t.Helper()
	b, _ := json.Marshal(n)
	return string(b)
}

// TestDeployDrainQueryUsesIndex is SIGMA-317. runDeployDrain runs the drain
// query every 3 seconds — 28,800 times a day — and on a healthy install it
// almost always finds nothing, because deploy_requests rows go 'queued' →
// 'drained' within one tick and then stay forever. With only
// deploy_requests_org_idx (org_id, created_at) on the table, no index can drive
// a predicate on (kind, status), so each of those 28,800 passes sequentially
// scanned the ENTIRE push history and took row locks under FOR UPDATE SKIP
// LOCKED to find zero work. The cost grew with the install's age and never
// shrank.
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
		  FROM generate_series(1, 20000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `ANALYZE deploy_requests`); err != nil {
		t.Fatal(err)
	}

	plan := explainPlan(t, st, false, store.DrainDeployRequestsQuery)
	if plan.seqScanOn("deploy_requests") {
		t.Fatalf("drain query sequentially scans deploy_requests; plan = %s", planJSON(t, plan))
	}
	if !plan.usesIndex("deploy_requests_queued_idx") {
		t.Fatalf("drain query does not use deploy_requests_queued_idx; plan = %s", planJSON(t, plan))
	}
}

// TestLatestPerResourceQueryUsesIndex is SIGMA-318. ListOrgDeployments's
// latest-per-resource query is unbounded by construction and the web mirror
// polls it every 30 seconds. It used to be a DISTINCT ON over the org's whole
// deployments table, and Postgres has no loose index scan, so answering it meant
// reading every deployment row the org had ever produced — service_status jsonb
// blobs included — to return one row per resource. The cost grew with deploy
// history forever and nothing in the UI hinted that age was the cause.
func TestLatestPerResourceQueryUsesIndex(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_plan_latest"

	// Enough history that a plan proportional to it is unmistakably distinct
	// from one proportional to the resource count: 12,000 rows to answer a
	// 200-row question.
	const resources, releasesEach = 200, 60

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachServer(ctx, orgID, env.ID, reg.Server.ID, "test"); err != nil {
		t.Fatal(err)
	}
	// 300 resources with a couple of years of releases behind them. Inserted
	// directly: going through the store's constructors would be correct but far
	// too slow, and this test is about the plan, not about how rows get written.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO resources (id, org_id, project_id, environment_id, server_id, name, kind, spec)
		SELECT 'res_plan_' || i, $1, $2, $3, $4, 'app' || i, 'app', '{"image":"nginx"}'::jsonb
		  FROM generate_series(1, $5) i`, orgID, proj.ID, env.ID, reg.Server.ID, resources); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, trigger,
		                         git_sha, status, created_by, created_at, service_status)
		SELECT 'dep_plan_' || i || '_' || j, $1, 'res_plan_' || i, $2, $3, 'git',
		       md5(j::text), 'success', 'test', now() - (j || ' hours')::interval,
		       '{"web":"success","api":"success"}'::jsonb
		  FROM generate_series(1, $4) i, generate_series(1, $5) j`,
		orgID, env.ID, reg.Server.ID, resources, releasesEach); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `ANALYZE deployments`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `ANALYZE resources`); err != nil {
		t.Fatal(err)
	}

	plan := explainPlan(t, st, true, store.LatestDeploymentPerResourceQuery, orgID)
	if plan.seqScanOn("deployments") {
		t.Fatalf("latest-per-resource query sequentially scans deployments; plan = %s", planJSON(t, plan))
	}
	if !plan.usesIndex("deployments_org_resource_idx") {
		t.Fatalf("latest-per-resource query does not use deployments_org_resource_idx; plan = %s",
			planJSON(t, plan))
	}
	// The substance of the ticket: the answer is 300 rows wide, so the plan must
	// touch on the order of 300 deployment rows, not the 39,000 the org has
	// accumulated. Slack of 2x leaves room for planner variation without letting
	// a history-proportional plan back in.
	if touched := plan.rowsTouched("deployments"); touched > 2*resources {
		t.Fatalf("latest-per-resource query read %.0f deployment rows to return %d; "+
			"cost is still proportional to deploy history. plan = %s",
			touched, resources, planJSON(t, plan))
	}

	// And the rewrite must not change the answer: one row per resource, newest
	// first, carrying every column the web mirror writes.
	feed, err := st.ListOrgDeployments(ctx, orgID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Latest) != resources {
		t.Fatalf("latest per resource = %d rows, want %d", len(feed.Latest), resources)
	}
	for _, d := range feed.Latest {
		// j = 1 is the newest (created_at = now() - 1 hour), so every resource's
		// latest row must be the one carrying md5('1').
		if d.GitSHA != md5Hex("1") {
			t.Fatalf("latest for %s = sha %q, want the newest release", d.ResourceID, d.GitSHA)
		}
		if d.ID == "" || d.OrgID != orgID || d.ResourceID == "" ||
			d.Status != "success" || d.CreatedBy != "test" || d.CreatedAt.IsZero() {
			t.Fatalf("latest row lost a column the web mirror reads: %+v", d)
		}
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
