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
	"errors"
	"strconv"
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

// hasFunctionScan reports whether the plan contains a Function Scan node — the
// signature of a set-returning function like jsonb_array_elements being run
// per row, which is exactly the cost SIGMA-365 removed from ResourceHostedHere.
func (n planNode) hasFunctionScan() bool {
	found := false
	n.walk(func(p planNode) {
		if p.NodeType == "Function Scan" {
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
	// Resources with a couple of years of releases behind them. Inserted
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

// TestDeployTargetsForServerUsesPartialIndex is SIGMA-332. Migration 0039 added
// deployments_server_target_idx (server_id, resource_id, created_at DESC) WHERE
// status IN (...) and says in its own comment that it makes "the render cost
// proportional to the resources on that server instead of to the install's total
// deploy history". A later commit added a third OR arm to the same WHERE — the
// Compose-placement check reading r.spec from the JOINED resources table — and
// because the disjunction was no longer a restriction on `deployments` alone,
// the partial index stopped being usable. The index still existed; it was just
// never chosen again, and nothing failed.
//
// This query runs once per server on the 60s fleet resync, once per push, once
// per resource mutation and once per deploy status report, so on a 200-server
// install the regression is worth millions of joined rows a minute.
func TestDeployTargetsForServerUsesPartialIndex(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_plan_targets"

	// A fleet, not a pair: one server's share of the history has to be a small
	// enough slice that an index scan is unambiguously the cheaper plan. That is
	// the shape SIGMA-188's index was added for and the shape the ticket
	// describes (200 servers), and it is the only shape in which the planner's
	// choice is evidence of anything — on a two-server install a seq scan
	// genuinely IS cheaper and the plan says nothing.
	const servers, resourcesPerServer, releasesEach = 40, 5, 50

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO git_connections (id, org_id, project_id, provider, installation_id, repo_full_name, created_by)
		VALUES ('gc_targets', $1, $2, 'github', 'inst', 'acme/targets', 'test')`, orgID, proj.ID); err != nil {
		t.Fatal(err)
	}
	serverIDs := make([]string, 0, servers)
	for i := 0; i < servers; i++ {
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
		serverIDs = append(serverIDs, reg.Server.ID)
	}
	// Every other server's resources are Compose apps that place a service on
	// the NEXT server in the ring, so the placement arm of the disjunction is
	// exercised rather than trivially empty.
	for i, srv := range serverIDs {
		spec := `{"image":"nginx"}`
		if i%2 == 1 {
			spec = `{"compose":{"services":[{"name":"web"},` +
				`{"name":"worker","serverId":"` + serverIDs[(i+1)%len(serverIDs)] + `"}]}}`
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO resources (id, org_id, project_id, environment_id, server_id, name, kind, spec)
			SELECT 'res_tgt_'||$5||'_'||j, $1, $2, $3, $4, 'app'||$5||'_'||j, 'app', $6::jsonb
			  FROM generate_series(1, $7) j`,
			orgID, proj.ID, env.ID, srv, strconv.Itoa(i), spec, resourcesPerServer); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id,
			                         trigger, git_sha, config_hash, status, created_by, created_at)
			SELECT 'dep_tgt_'||$4||'_'||j||'_'||k, $1, 'res_tgt_'||$4||'_'||j, $2, $3, 'gc_targets',
			       'git', md5(k::text), 'cfg', 'success', 'test', now() - (k||' hours')::interval
			  FROM generate_series(1, $5) j, generate_series(1, $6) k`,
			orgID, env.ID, srv, strconv.Itoa(i), resourcesPerServer, releasesEach); err != nil {
			t.Fatal(err)
		}
	}
	// The resources above were written with raw SQL, which skips
	// syncServicePlacementsTx; project their placements the way migration 0062's
	// backfill does. (TestComposePlacementsStayProjected covers the store paths
	// that maintain the table for real.)
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO resource_service_placements (resource_id, service, server_id)
		SELECT r.id, COALESCE(svc->>'name', ''), svc->>'serverId'
		  FROM resources r
		  CROSS JOIN LATERAL jsonb_array_elements(
		       CASE WHEN jsonb_typeof(r.spec->'compose'->'services') = 'array'
		            THEN r.spec->'compose'->'services' ELSE '[]'::jsonb END) svc
		 WHERE COALESCE(svc->>'serverId', '') <> ''
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"deployments", "resources", "resource_service_placements"} {
		if _, err := st.Pool.Exec(ctx, "ANALYZE "+tbl); err != nil {
			t.Fatal(err)
		}
	}

	// serverIDs[0] owns its own resources outright and hosts the placed 'worker'
	// service of the Compose resources owned by the last server in the ring.
	target := serverIDs[0]
	plan := explainPlan(t, st, true, store.DeployTargetsForServerQuery, target)
	if plan.seqScanOn("deployments") {
		t.Fatalf("DeployTargetsForServer sequentially scans deployments; plan = %s", planJSON(t, plan))
	}
	if !plan.usesIndex("deployments_server_target_idx") {
		t.Fatalf("DeployTargetsForServer no longer uses deployments_server_target_idx — SIGMA-188's "+
			"index is silently out of service; plan = %s", planJSON(t, plan))
	}
	// The substance: the render must read the rows this server is involved in,
	// not the fleet's. Two servers' worth of resources reach this one — its own
	// and the placed ones — out of forty. 2x slack for planner variation.
	const ownRows = 2 * resourcesPerServer * releasesEach
	if touched := plan.rowsTouched("deployments"); touched > 2*ownRows {
		t.Fatalf("DeployTargetsForServer read %.0f deployment rows for a server involved in %d; "+
			"cost is proportional to the fleet's deploy history. plan = %s",
			touched, ownRows, planJSON(t, plan))
	}

	// The plan change must not change WHO renders — the four ownership rules of
	// deploymentReporterClause depend on this staying exact.
	targets, err := st.DeployTargetsForServer(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2*resourcesPerServer {
		t.Fatalf("deploy targets for %s = %d resources, want %d owned + %d placed",
			target, len(targets), resourcesPerServer, resourcesPerServer)
	}
	// A server that owns nothing and hosts nothing renders nothing.
	spare, _, _, err := st.IssueBootstrapToken(ctx, orgID, "spare", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, spare, "spare", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := st.DeployTargetsForServer(ctx, reg.Server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("unattached server has %d deploy targets, want 0", len(empty))
	}
}

// TestResourceSpecsForServerAvoidsJsonbScan is SIGMA-365. ResourceHostedHere's
// Compose-placement arm used to be EXISTS(jsonb_array_elements(spec->'compose'->
// 'services') WHERE serverId = $) — an un-indexable set-returning function with
// no restriction on `resources` alone, so ResourceSpecsForServer (the single
// hottest read on the CP, run once per server on every 60s fleet resync) forced a
// cross-tenant Seq Scan of the whole resources table AND detoasted+parsed every
// non-owning tenant's spec jsonb per row. Rewritten to a semi-join on the indexed
// resource_service_placements. The regression marker is the Function Scan node
// reappearing, and nothing functional fails when it does — hence a plan test.
func TestResourceSpecsForServerAvoidsJsonbScan(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_plan_hosted"

	// A fleet, so one server's slice is small enough that the placement index is
	// unambiguously the cheaper access path than a jsonb scan of every row.
	const servers, resourcesPerServer = 40, 5

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	serverIDs := make([]string, 0, servers)
	for i := 0; i < servers; i++ {
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
		serverIDs = append(serverIDs, reg.Server.ID)
	}
	// Half the resources are Compose apps that place a service on the NEXT server
	// in the ring, so the placement arm is exercised, not trivially empty.
	for i, srv := range serverIDs {
		spec := `{"image":"nginx"}`
		if i%2 == 1 {
			spec = `{"compose":{"services":[{"name":"web"},` +
				`{"name":"worker","serverId":"` + serverIDs[(i+1)%len(serverIDs)] + `"}]}}`
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO resources (id, org_id, project_id, environment_id, server_id, name, kind, spec)
			SELECT 'res_h_'||$5||'_'||j, $1, $2, $3, $4, 'app'||$5||'_'||j, 'app', $6::jsonb
			  FROM generate_series(1, $7) j`,
			orgID, proj.ID, env.ID, srv, strconv.Itoa(i), spec, resourcesPerServer); err != nil {
			t.Fatal(err)
		}
	}
	// Raw inserts skip syncServicePlacementsTx; project placements as 0062 does.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO resource_service_placements (resource_id, service, server_id)
		SELECT r.id, COALESCE(svc->>'name', ''), svc->>'serverId'
		  FROM resources r
		  CROSS JOIN LATERAL jsonb_array_elements(
		       CASE WHEN jsonb_typeof(r.spec->'compose'->'services') = 'array'
		            THEN r.spec->'compose'->'services' ELSE '[]'::jsonb END) svc
		 WHERE COALESCE(svc->>'serverId', '') <> ''
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"resources", "resource_service_placements", "cluster_nodes"} {
		if _, err := st.Pool.Exec(ctx, "ANALYZE "+tbl); err != nil {
			t.Fatal(err)
		}
	}

	// The real ResourceSpecsForServer query, reconstructed from the shared rule.
	query := `
		SELECT r.id, r.project_id, r.kind, r.spec, r.ephemeral, COALESCE(r.cluster_id, ''),
		       COALESCE(r.public_label, ''),
		       COALESCE((SELECT sv.endpoint FROM servers sv WHERE sv.id = $1), '')
		  FROM resources r
		 WHERE` + store.ResourceHostedHere("$1") + `
		 ORDER BY r.created_at`
	target := serverIDs[0]
	plan := explainPlan(t, st, true, query, target)

	if plan.hasFunctionScan() {
		t.Fatalf("ResourceSpecsForServer runs a per-row set-returning function (jsonb scan is back, "+
			"SIGMA-365); plan = %s", planJSON(t, plan))
	}

	// The rewrite must not change WHO is hosted: server 0 owns its own resources
	// and hosts the placed 'worker' of the last server's Compose apps in the ring.
	specs, err := st.ResourceSpecsForServer(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2*resourcesPerServer {
		t.Fatalf("ResourceSpecsForServer(%s) = %d resources, want %d owned + %d placed",
			target, len(specs), resourcesPerServer, resourcesPerServer)
	}
}

// TestComposePlacementsStayProjected is the other half of SIGMA-332, and the
// half that can actually break something. Moving Compose placement out of the
// spec's jsonb and into resource_service_placements is only safe while the table
// is a faithful projection of every spec that has ever been written: a placement
// the table has not heard about is a service that renders in NO server's document
// and silently never deploys, which is precisely the failure the placement arm
// was added to prevent. So exercise the store paths that write a spec — create,
// and the placement editor — and check the render agrees with them each time.
func TestComposePlacementsStayProjected(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_placement_projection"

	home := connectServer(t, st, orgID, "home")
	away := connectServer(t, st, orgID, "away")
	proj, err := st.CreateProject(ctx, orgID, "p", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{home, away} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	// Created with the placement already in the spec — the shape a repo's
	// detected compose file arrives in, not only the shape the editor produces.
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: home, Name: "shop", Kind: "app",
		Spec: json.RawMessage(`{"compose":{"services":[{"name":"web"},` +
			`{"name":"worker","serverId":"` + away + `"}]}}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO git_connections (id, org_id, project_id, provider, installation_id, repo_full_name, created_by)
		VALUES ('gc_proj', $1, $2, 'github', 'inst', 'acme/shop', 'admin')`, orgID, proj.ID); err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: home, ConnectionID: "gc_proj",
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "sha1", ConfigHash: "cfg1",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	hostsIt := func(server string) bool {
		targets, err := st.DeployTargetsForServer(ctx, server)
		if err != nil {
			t.Fatal(err)
		}
		_, ok := targets[app.ID]
		return ok
	}
	if !hostsIt(away) {
		t.Fatal("CreateResource did not project the spec's Compose placement: the host of " +
			"'worker' renders nothing, so that service would never deploy")
	}
	// Reporting has to follow rendering, or the deploy runs and hangs forever.
	if _, _, _, err := st.DeploymentCloneCredential(ctx, away, dep.ID); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		t.Fatalf("clone credential for the placed host: %v", err)
	} else if errors.Is(err, store.ErrNotFound) {
		t.Fatal("the host of a placed service may render the deploy but not report on it; " +
			"deploymentReporterClause is no longer the mirror image of the render")
	}

	// Moving the service home must retract the projection, or `away` keeps
	// rendering a container that no longer belongs to it.
	if _, err := st.SetComposePlacements(ctx, orgID, app.ID, []store.ComposePlacement{
		{Service: "worker", ServerID: home},
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	if hostsIt(away) {
		t.Fatal("SetComposePlacements moved 'worker' home but the old host still renders it")
	}
	if !hostsIt(home) {
		t.Fatal("the home server lost its own deploy target")
	}
}
