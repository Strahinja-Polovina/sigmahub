package integration

// The two things the New Resource flow sends over HTTP that the HTTP layer used
// to throw away, against a real database and the real renderer.
//
// Both defects were invisible to every existing test because both live in the
// GAP between layers. gitdetect parsed the Compose graph correctly and the
// reconciler rendered per-service ops correctly — the graph just never travelled
// between them, so a five-service repo became one container (SIGMA-199). The
// store accepted CreateResourceInput.ClusterID and the cluster render worked
// end to end — the handler never decoded the field, so nothing outside the
// process could reach any of it (SIGMA-200). Unit tests on either side pass
// while the product does neither thing, which is why these drive the actual
// handler into the actual Postgres and then read the actual rendered document.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/gitdetect"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// composeRepo is a repository that describes FIVE services, deliberately
// covering every shape the renderer branches on: built from source with an
// explicit Dockerfile, built from a subdirectory with no ports (a worker),
// prebuilt images, a service holding a named volume, and one binding fixed host
// ports. The last two are the documented exceptions to zero-downtime deploys.
const composeRepo = `services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    ports:
      - "8080"
    depends_on:
      - db
      - cache
  worker:
    build: ./worker
  cache:
    image: redis:7.4
    ports:
      - "6379"
  db:
    image: postgres:16
    ports:
      - "5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
  proxy:
    image: traefik:v3
    ports:
      - "80:80"
      - "443:443"

volumes:
  pgdata:
`

// wizardAPI is the CP the dashboard actually talks to: the domain routes plus a
// live reconciler, so a create re-renders the document it should.
func wizardAPI(t *testing.T, st *store.Store, dsdKey ed25519.PrivateKey) (*httptest.Server, *reconciler.Reconciler, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := reconciler.New(log, st, dsdKey)
	srv := api.New(log, st, st, st, api.Options{
		DevServiceToken: "dev",
		DSDStore:        st,
		DSDWaiter:       rec,
		Reconcile:       rec,
		Clusters:        st,
		// Without this, PUT /resources/{id}/compose/placements answers 503
		// "compose placement is not configured" and the per-service placement
		// round trip — the whole point of storing the graph — goes untested.
		Compose:      st,
		DSDPublicKey: dsdKey.Public().(ed25519.PublicKey),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, rec, "dev"
}

// renderedOps returns a server's current ops, driving the reconciler until they
// satisfy want (or the deadline passes).
//
// The handler's own re-render runs asynchronously and reconciles are serialized
// per server — a contending caller SKIPS rather than blocks — so a single
// explicit Reconcile here could legitimately no-op while the handler's is still
// in flight. Retrying converges on the same state either way; the loop is about
// the race, not about flakiness tolerance.
func renderedOps(t *testing.T, st *store.Store, rec *reconciler.Reconciler, orgID, serverID string, want func([]dsd.Op) bool) []dsd.Op {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var ops []dsd.Op
	for {
		if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
			t.Fatalf("reconcile %s: %v", serverID, err)
		}
		cur, err := st.GetDSD(ctx, serverID)
		if err == nil {
			ops = cur.Document.Ops
			if want(ops) {
				return ops
			}
		}
		if time.Now().After(deadline) {
			return ops
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func opIDs(ops []dsd.Op) map[string]string {
	out := make(map[string]string, len(ops))
	for _, op := range ops {
		out[op.ID] = op.Kind
	}
	return out
}

// A repo whose compose file declares five services must be created as five
// services and render five workloads. The wizard used to send only ports and a
// health check, so `spec.compose` was never written; the reconciler then took
// the single-container branch and the other four services were never built,
// never started, and never mentioned in any error.
func TestComposeRepoDeploysEveryServiceItDeclares(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	ts, rec, token := wizardAPI(t, st, dsdKey)
	orgID := "org_compose_wizard"

	proj, err := st.CreateProject(ctx, orgID, "shop", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	serverID := connectServer(t, st, orgID, "host-a")
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "admin"); err != nil {
		t.Fatal(err)
	}

	// The graph the dashboard receives is whatever gitdetect emits — not a
	// hand-written guess at it. Deriving the request body from the real detector
	// is what makes this a test of the BOUNDARY: if the two shapes ever diverge,
	// the spec below stops carrying what the renderer reads.
	detected := gitdetect.Detect(map[string][]byte{"docker-compose.yml": []byte(composeRepo)})
	if !detected.HasCompose || len(detected.Services) != 5 {
		t.Fatalf("fixture no longer detects a five-service graph: %+v", detected.Services)
	}

	spec := map[string]any{
		"repo":        "acme/shop",
		"ports":       []map[string]any{{"container": 8080, "host": 0, "protocol": "tcp"}},
		"healthCheck": detected.HealthCheck,
		"compose":     map[string]any{"services": detected.Services},
	}
	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": env.ID, "serverId": serverID, "name": "shop", "kind": "app", "spec": spec,
	})
	if code != http.StatusCreated {
		t.Fatalf("create from a compose repo → %d: %v", code, body)
	}
	resourceID, _ := body["id"].(string)
	if resourceID == "" {
		t.Fatalf("create returned no resource id: %v", body)
	}

	// Stored: every service the repository declares, with the fields the build
	// and the rollout are made of. A name that survived while its build context
	// did not is a service that builds the wrong thing.
	stored := storedComposeServices(t, st, orgID, env.ID, resourceID)
	if len(stored) != len(detected.Services) {
		t.Fatalf("stored %d of %d detected services: %+v", len(stored), len(detected.Services), stored)
	}
	for _, want := range detected.Services {
		got, ok := stored[want.Name]
		if !ok {
			t.Fatalf("service %q never reached the stored spec (have %v)", want.Name, keysOf(stored))
		}
		if got.Build != want.Build || got.Dockerfile != want.Dockerfile || got.Image != want.Image {
			t.Errorf("service %q: stored build/dockerfile/image = %q/%q/%q, detected %q/%q/%q",
				want.Name, got.Build, got.Dockerfile, got.Image, want.Build, want.Dockerfile, want.Image)
		}
		if got.Rollout != want.Rollout {
			t.Errorf("service %q: stored rollout %q, detected %q", want.Name, got.Rollout, want.Rollout)
		}
		if len(got.Ports) != len(want.Ports) {
			t.Errorf("service %q: stored ports %v, detected %v", want.Name, got.Ports, want.Ports)
		}
		if len(got.DependsOn) != len(want.DependsOn) {
			t.Errorf("service %q: stored dependsOn %v, detected %v", want.Name, got.DependsOn, want.DependsOn)
		}
	}
	// And the evidence behind a recreate verdict, so the dashboard can say WHY a
	// service is exempt from the zero-downtime promise instead of just badging it.
	if len(stored["db"].NamedVolumes) == 0 {
		t.Errorf("db lost the named volume that forced its recreate rollout: %+v", stored["db"])
	}
	if len(stored["proxy"].PublishedPorts) == 0 {
		t.Errorf("proxy lost the fixed host ports that forced its recreate rollout: %+v", stored["proxy"])
	}

	// Surviving an edit: placement rewrites the compose block through its own
	// struct, so a field that struct has no home for is DELETED the first time
	// anyone moves a service. `dockerfile` was exactly that — written here,
	// consumed by the build op, and gone after one drag in the UI.
	if _, err := st.SetComposePlacements(ctx, orgID, resourceID,
		[]store.ComposePlacement{{Service: "worker", ServerID: serverID}}, "admin"); err != nil {
		t.Fatal(err)
	}
	after := storedComposeServices(t, st, orgID, env.ID, resourceID)
	if after["web"].Dockerfile != stored["web"].Dockerfile {
		t.Errorf("placement edit dropped web's dockerfile: %q → %q",
			stored["web"].Dockerfile, after["web"].Dockerfile)
	}
	if len(after["db"].NamedVolumes) != len(stored["db"].NamedVolumes) ||
		len(after["proxy"].PublishedPorts) != len(stored["proxy"].PublishedPorts) {
		t.Errorf("placement edit dropped the recreate evidence: db=%+v proxy=%+v", after["db"], after["proxy"])
	}
	if after["db"].Rollout != gitdetect.RolloutRecreate {
		t.Errorf("placement edit changed db's rollout to %q", after["db"].Rollout)
	}

	// Rendered: a git deploy of this resource must produce ops PER SERVICE.
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/shop",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: resourceID, EnvironmentID: env.ID, ServerID: serverID,
		ConnectionID: conn.ID, Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin"); err != nil {
		t.Fatal(err)
	}

	wantRollouts := len(detected.Services)
	ops := renderedOps(t, st, rec, orgID, serverID, func(ops []dsd.Op) bool {
		n := 0
		for _, op := range ops {
			if strings.HasPrefix(op.ID, "res:"+resourceID+":") {
				n++
			}
		}
		return n == wantRollouts
	})
	kinds := opIDs(ops)

	for _, svc := range detected.Services {
		// Source-built services get their own image.build; prebuilt ones their own
		// image.pull. One shared build for the whole repo is the single-container
		// behaviour this replaces.
		if svc.Build != "" {
			if kinds["build:"+resourceID+":"+svc.Name] != dsd.KindImageBuild {
				t.Errorf("service %q has no build op of its own; ops = %v", svc.Name, kinds)
			}
		} else if kinds["pull:"+resourceID+":"+svc.Name] != dsd.KindImagePull {
			t.Errorf("service %q has no image pull of its own; ops = %v", svc.Name, kinds)
		}
		// The swap class the detector decided has to be the op kind that ships.
		wantKind := dsd.KindDeployRollout
		if svc.Rollout == gitdetect.RolloutRecreate {
			wantKind = dsd.KindDeployRecreate
		}
		if got := kinds["res:"+resourceID+":"+svc.Name]; got != wantKind {
			t.Errorf("service %q rendered as %q, want %q (rollout %q)", svc.Name, got, wantKind, svc.Rollout)
		}
	}
	// The bug's signature: one op for the whole app. A `res:<id>` with no service
	// suffix means the renderer took the single-container path.
	if _, single := kinds["res:"+resourceID]; single {
		t.Errorf("the compose app still rendered as one container; ops = %v", kinds)
	}
}

// The same repo WITHOUT its service graph is the old behaviour, kept as the
// control: one container, no per-service ops. It is what every compose repo
// looked like while the web layer dropped the graph.
func TestAppWithNoComposeGraphStillRendersOneContainer(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	ts, rec, token := wizardAPI(t, st, dsdKey)
	orgID := "org_single"

	proj, err := st.CreateProject(ctx, orgID, "shop", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	serverID := connectServer(t, st, orgID, "host-a")
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "admin"); err != nil {
		t.Fatal(err)
	}

	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": env.ID, "serverId": serverID, "name": "shop", "kind": "app",
		"spec": map[string]any{"image": "nginx:1.27", "ports": []map[string]any{{"container": 80}}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create → %d: %v", code, body)
	}
	resourceID, _ := body["id"].(string)

	ops := renderedOps(t, st, rec, orgID, serverID, func(ops []dsd.Op) bool {
		return opIDs(ops)["res:"+resourceID] != ""
	})
	kinds := opIDs(ops)
	if kinds["res:"+resourceID] != dsd.KindContainerApply {
		t.Fatalf("single-container app rendered as %q; ops = %v", kinds["res:"+resourceID], kinds)
	}
	for id := range kinds {
		if strings.HasPrefix(id, "res:"+resourceID+":") {
			t.Fatalf("an app with no compose graph rendered a per-service op %q", id)
		}
	}
}

// storedComposeServices reads back what the create actually persisted, keyed by
// service name.
func storedComposeServices(t *testing.T, st *store.Store, orgID, envID, resourceID string) map[string]gitdetect.ComposeService {
	t.Helper()
	resources, err := st.ListResources(context.Background(), orgID, envID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources {
		if r.ID != resourceID {
			continue
		}
		var shape struct {
			Compose *struct {
				Services []gitdetect.ComposeService `json:"services"`
			} `json:"compose"`
		}
		if err := json.Unmarshal(r.Spec, &shape); err != nil {
			t.Fatalf("stored spec is not decodable: %v (%s)", err, r.Spec)
		}
		if shape.Compose == nil {
			t.Fatalf("the stored spec has no compose block at all: %s", r.Spec)
		}
		out := map[string]gitdetect.ComposeService{}
		for _, svc := range shape.Compose.Services {
			out[svc.Name] = svc
		}
		return out
	}
	t.Fatalf("resource %s not found in %s", resourceID, envID)
	return nil
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A cluster deploy has to be reachable from outside the process, and every rule
// the store enforces has to be enforced (and explained) at the HTTP boundary.
func TestClusterDeployOverHTTP(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	ts, rec, token := wizardAPI(t, st, dsdKey)
	orgID := "org_cluster_http"

	envID, cpServer, worker := clusterFixture(t, st, orgID)
	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}

	// The create the dashboard makes: a cluster id and no server at all.
	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": envID, "clusterId": cluster.ID, "name": "api", "kind": "app",
		"spec": map[string]any{"image": "nginx:1.27", "ports": []map[string]any{{"container": 80}}},
	})
	if code != http.StatusCreated {
		t.Fatalf("cluster-targeted create → %d: %v", code, body)
	}
	resourceID, _ := body["id"].(string)
	if resourceID == "" {
		t.Fatalf("create returned no resource id: %v", body)
	}
	// A cluster workload is scheduled, so it must NOT be pinned to a host.
	if sid, _ := body["serverId"].(string); sid != "" {
		t.Fatalf("a cluster workload was pinned to server %q", sid)
	}

	// The control-plane node is the only one that applies workloads, and it is
	// also the one the create has no server_id to point at — so this asserts both
	// that the workload renders and that the handler found the node to re-render.
	ops := renderedOps(t, st, rec, orgID, cpServer, func(ops []dsd.Op) bool {
		return opIDs(ops)["res:"+resourceID] == dsd.KindK8sApply
	})
	kinds := opIDs(ops)
	if kinds["res:"+resourceID] != dsd.KindK8sApply {
		t.Fatalf("the control-plane node does not render its workload; ops = %v", kinds)
	}
	if kinds["k8s:node:"+cluster.ID] != dsd.KindK8sNode {
		t.Fatalf("the control-plane node lost its membership op; ops = %v", kinds)
	}
	// A worker applies nothing: rendering the same Deployment on every node would
	// create competing appliers of one object.
	workerOps := renderedOps(t, st, rec, orgID, worker, func([]dsd.Op) bool { return true })
	if _, applied := opIDs(workerOps)["res:"+resourceID]; applied {
		t.Fatalf("a worker node also renders the workload; ops = %v", opIDs(workerOps))
	}

	// A stateful kind aimed at a cluster is refused with the REASON — a database
	// rescheduled onto a node without its volume is data loss, not a slow deploy.
	// It used to reach the client as a bare 500 "internal error", because the
	// store's refusal is its own error type and the HTTP mapper only knew about
	// ErrInvalid.
	code, body = postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": envID, "clusterId": cluster.ID, "name": "db", "kind": "postgres",
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("postgres into a cluster → %d: %v", code, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "postgres") || !strings.Contains(msg, "own server") {
		t.Fatalf("refusal %q does not say what is wrong or where a database belongs", msg)
	}

	// Both targets is incoherent, and saying so beats silently preferring one.
	code, body = postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": envID, "clusterId": cluster.ID, "serverId": cpServer,
		"name": "both", "kind": "app",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("server AND cluster → %d: %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "not both") {
		t.Fatalf("refusal %q does not name the conflict", msg)
	}

	// Neither is the case that used to answer "serverId is required" for a
	// perfectly good cluster deploy. The message must offer both options.
	code, body = postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": envID, "name": "nowhere", "kind": "app",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("no target → %d: %v", code, body)
	}
	msg, _ = body["error"].(string)
	if !strings.Contains(msg, "serverId") || !strings.Contains(msg, "clusterId") {
		t.Fatalf("refusal %q names only one of the two ways to target a deploy", msg)
	}

	// The server-type matrix still governs the server path, and still reaches the
	// client as a 422 that names the allowed types rather than a shape error.
	buildOnly := connectTypedServer(t, st, orgID, "builder", "build")
	if err := st.AttachServer(ctx, orgID, envID, buildOnly, "admin"); err != nil {
		t.Fatal(err)
	}
	code, body = postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": envID, "serverId": buildOnly, "name": "onbuilder", "kind": "app",
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("app on a build server → %d: %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "build") {
		t.Fatalf("matrix refusal %q does not name the server type it refused", msg)
	}
}

// A cluster in another environment is not this environment's cluster, and a
// cluster id from another org is not visible at all. Both are the tenant
// boundary a newly-decoded, client-supplied id has to be held to.
func TestClusterTargetIsScopedToItsEnvironmentAndOrg(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	ts, _, token := wizardAPI(t, st, dsdKey)
	orgID := "org_cluster_scope"

	envID, cpServer, _ := clusterFixture(t, st, orgID)
	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateEnvironment(ctx, orgID, projectID, "staging", false, "admin")
	if err != nil {
		t.Fatal(err)
	}

	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/resources", map[string]any{
		"environmentId": other.ID, "clusterId": cluster.ID, "name": "api", "kind": "app",
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("cluster from another environment → %d: %v", code, body)
	}

	// Another org's cluster id must not resolve, or a client-supplied id would be
	// a way to plant a workload in someone else's cluster.
	code, body = postAs(t, ts, token, "/v1/orgs/org_someone_else/resources", map[string]any{
		"environmentId": envID, "clusterId": cluster.ID, "name": "api", "kind": "app",
	})
	if code != http.StatusNotFound {
		t.Fatalf("cross-org cluster id → %d: %v", code, body)
	}
}
