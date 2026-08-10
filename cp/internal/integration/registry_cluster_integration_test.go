package integration

// The image registry, cluster status reporting, and who is allowed to advance a
// deployment — all against a real database.
//
// Every defect this file was written after was hand-written SQL that no test
// executed: a NOT NULL the code wrote NULL into, a status column nothing ever
// updated, a WHERE clause that silently dropped legitimate reports. Compiling
// and vetting said nothing about any of them.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestImageRegistryLifecycle(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_registry"

	if _, ok, err := st.GetImageRegistry(ctx, orgID); err != nil || ok {
		t.Fatalf("a fresh org must have no registry: ok=%v err=%v", ok, err)
	}

	reg, err := st.SetImageRegistry(ctx, orgID, store.SetImageRegistryInput{
		Host: "ghcr.io", Namespace: "acme", Username: "bot", Password: "s3cret",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Repository() != "ghcr.io/acme" {
		t.Fatalf("repository = %q", reg.Repository())
	}
	if !reg.HasPassword {
		t.Fatal("the stored credential must be reported as present")
	}

	// The reconciler reads only the prefix; it must match what the API showed.
	repo, err := st.ImageRepositoryForOrg(ctx, orgID)
	if err != nil || repo != "ghcr.io/acme" {
		t.Fatalf("ImageRepositoryForOrg = %q err = %v", repo, err)
	}

	// Editing the namespace with no password must KEEP the stored one. Clearing
	// it silently would break every push an hour later, with nothing in the UI
	// to explain why.
	if _, err := st.SetImageRegistry(ctx, orgID, store.SetImageRegistryInput{
		Host: "ghcr.io", Namespace: "acme-prod", Username: "bot",
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	// The credential is released on NEED, not on membership (SIGMA-258), so the
	// reader has to be a server with something to build: an in-flight deployment
	// naming it as the build server.
	serverID := connectServer(t, st, orgID, "builder")
	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "admin"); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: serverID,
		ConnectionID: conn.ID, Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, dep.ID, serverID); err != nil {
		t.Fatal(err)
	}
	cred, err := st.RegistryCredentialForServer(ctx, orgID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Password != "s3cret" || cred.Username != "bot" || cred.Host != "ghcr.io" {
		t.Fatalf("credential did not survive the edit: %+v", cred)
	}

	// Releasing a push credential is exactly the access that must leave a trail.
	entries, err := st.ListAudit(ctx, orgID, 50)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	for _, e := range entries {
		if e.Action == "Registry credential released" {
			released = true
		}
	}
	if !released {
		t.Fatal("credential release is not audited")
	}

	// A pasted URL is the most common mistake and would silently produce a
	// different repository than the one configured.
	for _, bad := range []string{"https://ghcr.io", "ghcr.io/acme", "reg istry"} {
		if _, err := st.SetImageRegistry(ctx, orgID, store.SetImageRegistryInput{Host: bad}, "admin"); err == nil {
			t.Fatalf("host %q must be refused", bad)
		}
	}

	if err := st.DeleteImageRegistry(ctx, orgID, "admin"); err != nil {
		t.Fatal(err)
	}
	if repo, _ := st.ImageRepositoryForOrg(ctx, orgID); repo != "" {
		t.Fatalf("registry survived delete: %q", repo)
	}
	if err := st.DeleteImageRegistry(ctx, orgID, "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

// A cluster used to be written as 'provisioning' at creation and never moved:
// nothing called the one function that could advance it. A cluster that came up
// perfectly and one whose install failed on the first line were the same thing
// in the product.
func TestClusterStatusFollowsItsNodes(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cluster_status"
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
	if cluster.Status != "provisioning" {
		t.Fatalf("new cluster status = %q", cluster.Status)
	}

	// A worker reporting first cannot make the cluster ready: without an API
	// server there is nothing to schedule onto.
	if _, status, err := st.ReportClusterNode(ctx, worker, store.ClusterNodeReport{Ready: true}); err != nil || status != "provisioning" {
		t.Fatalf("worker-only report: status=%q err=%v", status, err)
	}

	// The control plane comes up: ready, with the endpoint kubectl dials.
	_, status, err := st.ReportClusterNode(ctx, cpServer, store.ClusterNodeReport{
		Ready: true, APIEndpoint: "https://10.8.0.2:6443", Version: "v1.31.4+k3s1",
	})
	if err != nil || status != "ready" {
		t.Fatalf("control-plane report: status=%q err=%v", status, err)
	}
	list, err := st.ListClusters(ctx, orgID, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("clusters = %+v err=%v", list, err)
	}
	if list[0].Status != "ready" || list[0].APIEndpoint != "https://10.8.0.2:6443" || list[0].Version != "v1.31.4+k3s1" {
		t.Fatalf("cluster readout = %+v", list[0])
	}
	for _, n := range list[0].Nodes {
		if n.NodeStatus != store.NodeStatusReady {
			t.Fatalf("node %s status = %q, want ready", n.ServerID, n.NodeStatus)
		}
		if n.ReportedAt == nil {
			t.Fatalf("node %s has no report timestamp", n.ServerID)
		}
	}

	// A node that breaks degrades the cluster and says why — the message is the
	// entire difference between "something is wrong" and a fixable problem.
	if _, status, err := st.ReportClusterNode(ctx, worker, store.ClusterNodeReport{
		Ready: false, Message: "k3s-agent failed to start: no route to the control plane",
	}); err != nil || status != "degraded" {
		t.Fatalf("failed worker: status=%q err=%v", status, err)
	}
	list, _ = st.ListClusters(ctx, orgID, "")
	var workerNode store.ClusterNode
	for _, n := range list[0].Nodes {
		if n.ServerID == worker {
			workerNode = n
		}
	}
	if workerNode.NodeStatus != store.NodeStatusError || workerNode.NodeMessage == "" {
		t.Fatalf("worker node readout = %+v", workerNode)
	}

	// A control plane that stops answering takes the cluster back to
	// provisioning: an endpoint from a dead API server is worse than none,
	// because it is what kubectl would be told to dial.
	if _, status, err := st.ReportClusterNode(ctx, cpServer, store.ClusterNodeReport{
		Ready: false, Message: "the API server is not answering yet",
	}); err != nil || status != "provisioning" {
		t.Fatalf("failed control plane: status=%q err=%v", status, err)
	}

	// A server in no cluster has nothing to report about.
	stray := connectServer(t, st, orgID, "stray")
	if _, _, err := st.ReportClusterNode(ctx, stray, store.ClusterNodeReport{Ready: true}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("report from a non-member err = %v, want ErrNotFound", err)
	}
}

// Who may advance a deployment. This is the mirror of DeployTargetsForServer:
// a server that RENDERS a deployment's ops must be allowed to REPORT on them,
// and every gap between the two is a deploy that runs and then hangs forever
// because its status report was silently dropped.
func TestDeploymentReportersMatchWhoRendersTheOps(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_reporters"

	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	runServer := connectServer(t, st, orgID, "run")
	buildServer := connectServer(t, st, orgID, "build")
	for _, id := range []string{runServer, buildServer} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: runServer, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: runServer,
		ConnectionID: conn.ID, Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, dep.ID, buildServer); err != nil {
		t.Fatal(err)
	}

	// The build lives in the BUILD server's document, so the build server is the
	// one that reports it. Scoping the report to the deploy target dropped it,
	// leaving the deployment in 'queued' — and the rollout, which is gated on
	// the deployment leaving 'queued', never rendered. A deadlock, silently.
	if err := st.AdvanceDeploymentForResource(ctx, buildServer, app.ID, "clone", true, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceDeploymentForResource(ctx, buildServer, app.ID, "build", true, "", 0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDeployment(ctx, orgID, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "deploying" {
		t.Fatalf("after the build server reported, status = %q, want deploying", got.Status)
	}

	// The deploy target finishes it.
	if err := st.AdvanceDeploymentForResource(ctx, runServer, app.ID, "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetDeployment(ctx, orgID, dep.ID); got.Status != "success" {
		t.Fatalf("status = %q, want success", got.Status)
	}

	// An unrelated server must not be able to move someone else's deployment.
	other := connectServer(t, st, orgID, "elsewhere")
	dep2, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: runServer,
		ConnectionID: conn.ID, Trigger: "manual", GitRef: "main", GitSHA: "def7654321",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceDeploymentForResource(ctx, other, app.ID, "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetDeployment(ctx, orgID, dep2.ID); got.Status != "queued" {
		t.Fatalf("an unrelated server advanced a deployment to %q", got.Status)
	}
}

// Everything a server needs to render a PLACED Compose service has to agree on
// which resources it hosts. These are read separately and combined, so when
// only some of them included placed resources the placement host got a deploy
// target for a resource it had no spec for, rendered nothing, and the service
// never started — with no error anywhere.
func TestPlacedComposeServiceIsVisibleToItsHost(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_placement"

	proj, err := st.CreateProject(ctx, orgID, "shop", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	owner := connectServer(t, st, orgID, "owner")
	placed := connectServer(t, st, orgID, "placed")
	stranger := connectServer(t, st, orgID, "stranger")
	for _, id := range []string{owner, placed, stranger} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}

	spec, err := json.Marshal(map[string]any{"compose": map[string]any{"services": []map[string]any{
		{"name": "db", "image": "postgres:16", "serverId": placed},
		{"name": "web", "image": "nginx:1.27", "dependsOn": []string{"db"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: owner, Name: "shopapp", Kind: "app", Spec: spec,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{
		ProjectID: proj.ID, EnvironmentID: env.ID, Name: "DATABASE_PASSWORD", Value: "hunter2", EnvVar: true,
	}); err != nil {
		t.Fatal(err)
	}

	// The host of a placed service must see the resource, or it has nothing to
	// render the service from.
	for _, c := range []struct {
		name   string
		server string
		want   bool
	}{
		{"owner", owner, true},
		{"placement host", placed, true},
		{"unrelated server", stranger, false},
	} {
		specs, err := st.ResourceSpecsForServer(ctx, c.server)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range specs {
			if s.ResourceID == res.ID {
				found = true
			}
		}
		if found != c.want {
			t.Fatalf("%s sees the resource = %v, want %v", c.name, found, c.want)
		}
	}

	// And its secrets, or the service starts without the credentials it needs.
	refs, err := st.SecretRefsForServer(ctx, placed)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs[res.ID]) == 0 {
		t.Fatalf("the placement host got no secret refs for the resource: %+v", refs)
	}
	if strangerRefs, _ := st.SecretRefsForServer(ctx, stranger); len(strangerRefs[res.ID]) != 0 {
		t.Fatal("an unrelated server must not be handed the resource's secret refs")
	}
}

// The value-fetch path has to grant exactly what the reference path rendered.
//
// A resource's secret references go into a host's document from one query and
// the values come back through another. When the second was narrower, the host
// was handed a reference and then refused the value — the apply failed with
// "resolve secrets" and nothing said why. That is reachable two ways: a cluster
// workload belongs to no server, and a placed Compose service belongs to a
// different one.
func TestSecretFetchMatchesWhatWasRendered(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_secretfetch"
	envID, cpServer, worker := clusterFixture(t, st, orgID)

	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{
		ProjectID: projectID, EnvironmentID: envID, Name: "DATABASE_URL",
		Value: "postgres://u:p@db/app", EnvVar: true,
	}); err != nil {
		t.Fatal(err)
	}
	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ClusterID: cluster.ID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:1.27","ports":[{"container":80}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Rendered: the control-plane node must be told the workload has a secret,
	// or the manifest carries an empty Secret and the app starts unconfigured.
	refs, err := st.SecretRefsForServer(ctx, cpServer)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs[app.ID]) == 0 {
		t.Fatalf("the control-plane node got no secret refs for its cluster workload: %+v", refs)
	}

	// Fetched: and it must actually be able to resolve them.
	resolved, err := st.ResolveSecretsForResource(ctx, orgID, cpServer, app.ID, "agent:"+cpServer)
	if err != nil {
		t.Fatalf("the node that renders the workload cannot fetch its secrets: %v", err)
	}
	found := false
	for _, r := range resolved {
		if r.Name == "DATABASE_URL" && r.Value == "postgres://u:p@db/app" {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret did not resolve: %+v", resolved)
	}

	// A server in neither the cluster nor the placement set must still be
	// refused — the scoping is what stops a stolen agent token draining the org.
	stranger := connectServer(t, st, orgID, "stranger")
	if _, err := st.ResolveSecretsForResource(ctx, orgID, stranger, app.ID, "agent:"+stranger); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unrelated server resolved another resource's secrets: err = %v", err)
	}
}

// A push that resolves to nothing has to say so.
//
// Every drained request was marked 'drained' whether it produced deployments or
// not, so a push into an environment with no app resources — the normal state
// right after connecting a repo — looked exactly like a successful one. The
// webhook was accepted, the request was drained, and no deploy ever ran, with
// nothing in the product to explain it.
func TestPushWithNoTargetsSaysSo(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_push"

	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	server := connectServer(t, st, orgID, "host")
	if err := st.AttachServer(ctx, orgID, env.ID, server, "admin"); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/web",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// A push arrives. Inserted directly because the enqueue is tx-internal to
	// the webhook path; the row is what the drain actually reads.
	enqueuePush := func(id, sha string) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO deploy_requests (id, org_id, connection_id, environment_id, kind, ref, sha, branch, status)
			VALUES ($1,$2,$3,$4,'deploy','refs/heads/main',$5,'main','queued')`,
			id, orgID, conn.ID, env.ID, sha); err != nil {
			t.Fatal(err)
		}
	}

	// Nothing to deploy yet.
	enqueuePush("dpr_first", "abc1234567")
	if _, err := st.DrainDeployRequests(ctx); err != nil {
		t.Fatal(err)
	}
	reqs, err := st.ListDeployRequests(ctx, orgID, 10)
	if err != nil || len(reqs) != 1 {
		t.Fatalf("deploy requests = %+v err=%v", reqs, err)
	}
	if reqs[0].Status != "no_targets" {
		t.Fatalf("a push that deployed nothing is recorded as %q", reqs[0].Status)
	}
	if reqs[0].DeploymentsCreated != 0 || reqs[0].Detail == "" {
		t.Fatalf("the outcome must explain itself: %+v", reqs[0])
	}

	// Now there is something to deploy, and the same push shape must read
	// differently — otherwise the distinction is decoration.
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: server, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:1.27"}`),
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	enqueuePush("dpr_second", "def7654321")
	if _, err := st.DrainDeployRequests(ctx); err != nil {
		t.Fatal(err)
	}
	reqs, _ = st.ListDeployRequests(ctx, orgID, 10)
	var latest store.DeployRequest
	for _, r := range reqs {
		if r.SHA == "def7654321" {
			latest = r
		}
	}
	if latest.Status != "drained" || latest.DeploymentsCreated != 1 {
		t.Fatalf("a push that deployed is recorded as %+v", latest)
	}
}

// A resource's status has to be writable by the host that runs it. The two
// kinds that belong to no single server — a cluster workload and a placed
// Compose service — reported into nothing, so the dashboard showed them
// provisioning forever while they were running fine.
func TestResourceStatusAcceptedFromTheHostThatRunsIt(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	orgID := "org_resstatus"
	envID, cpServer, worker := clusterFixture(t, st, orgID)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ClusterID: cluster.ID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:1.27"}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// A status report is only accepted against a DSD the CP has issued.
	if _, _, err := st.StoreDSD(ctx, orgID, cpServer, []dsd.Op{{
		ID: "res:" + app.ID, Kind: dsd.KindK8sApply, Spec: json.RawMessage(`{}`),
	}}, "hash-1", dsdKey); err != nil {
		t.Fatal(err)
	}

	reported := json.RawMessage(`{"state":"failed","error":"ImagePullBackOff"}`)
	if _, err := st.ApplyDSDStatus(ctx, cpServer, 1,
		map[string]json.RawMessage{app.ID: reported}, false, nil); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListResources(ctx, orgID, envID)
	if err != nil {
		t.Fatal(err)
	}
	var got store.Resource
	for _, r := range list {
		if r.ID == app.ID {
			got = r
		}
	}
	if !strings.Contains(string(got.Status), "ImagePullBackOff") {
		t.Fatalf("the cluster node's status report did not land: %s", got.Status)
	}

	// A node of the same cluster is legitimate; a server outside it is not.
	if _, _, err := st.StoreDSD(ctx, orgID, worker, []dsd.Op{{
		ID: "res:" + app.ID, Kind: dsd.KindK8sApply, Spec: json.RawMessage(`{}`),
	}}, "hash-w", dsdKey); err != nil {
		t.Fatal(err)
	}
	stranger := connectServer(t, st, orgID, "stranger")
	if _, _, err := st.StoreDSD(ctx, orgID, stranger, []dsd.Op{{
		ID: "res:" + app.ID, Kind: dsd.KindResourceSync, Spec: json.RawMessage(`{}`),
	}}, "hash-s", dsdKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyDSDStatus(ctx, stranger, 1,
		map[string]json.RawMessage{app.ID: json.RawMessage(`{"state":"running"}`)}, true, nil); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListResources(ctx, orgID, envID)
	for _, r := range list {
		if r.ID == app.ID && !strings.Contains(string(r.Status), "ImagePullBackOff") {
			t.Fatalf("an unrelated server overwrote the resource's status: %s", r.Status)
		}
	}
}

// A certificate report has to be accepted from the host that actually rendered
// the domain, or the state never leaves 'pending' on exactly the deploys that
// span more than one machine.
func TestCertStatusAcceptedFromTheHostThatRendersTheDomain(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cert"

	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	owner := connectServer(t, st, orgID, "owner")
	placed := connectServer(t, st, orgID, "placed")
	stranger := connectServer(t, st, orgID, "stranger")
	for _, id := range []string{owner, placed, stranger} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := json.Marshal(map[string]any{"compose": map[string]any{"services": []map[string]any{
		{"name": "web", "image": "nginx:1.27", "ports": []int{8080}, "serverId": placed},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: owner, Name: "site", Kind: "app", Spec: spec,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// The owning server has to carry the proxy role before a domain may attach.
	if err := st.SetProxyRole(ctx, orgID, owner, true, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AttachDomain(ctx, orgID, res.ID, "app.example.com", "http", "admin"); err != nil {
		t.Fatal(err)
	}

	// The placement host renders the domain, so it is the one terminating TLS.
	if doms, _ := st.DomainsForServer(ctx, placed); len(doms[res.ID]) == 0 {
		t.Fatal("the placement host was not given the domain it has to route")
	}
	if err := st.SetDomainCertStatus(ctx, placed, "app.example.com", "issued", "01ab", nil, ""); err != nil {
		t.Fatalf("the host that renders the domain cannot report its certificate: %v", err)
	}
	doms, err := st.ListDomainsForResource(ctx, orgID, res.ID)
	if err != nil || len(doms) != 1 || doms[0].CertStatus != "issued" {
		t.Fatalf("cert status = %+v (err %v)", doms, err)
	}

	// An unrelated host must not be able to write cert state for it.
	if err := st.SetDomainCertStatus(ctx, stranger, "app.example.com", "failed", "", nil, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unrelated server wrote another host's certificate state: err = %v", err)
	}
}

// A Compose deploy completes when every declared service succeeds, so the
// denominator has to be recorded when the deployment is created. The webhook
// path computed it and the manual path did not, so a manual deploy or a
// rollback ran perfectly and then sat in 'deploying' forever — which also keeps
// the release out of the rollback targets, since those require a success.
func TestComposeDeploymentRecordsItsServiceCount(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_svccount"

	proj, err := st.CreateProject(ctx, orgID, "shop", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	server := connectServer(t, st, orgID, "host")
	if err := st.AttachServer(ctx, orgID, env.ID, server, "admin"); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(map[string]any{"compose": map[string]any{"services": []map[string]any{
		{"name": "db", "image": "postgres:16"},
		{"name": "web", "image": "nginx:1.27"},
		// Neither a build context nor an image: not runnable, so it must not be
		// counted or the deployment waits on a service nothing renders.
		{"name": "broken"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: server, Name: "shopapp", Kind: "app", Spec: spec,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/shop",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: server, ConnectionID: conn.ID,
		Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if dep.ServiceCount != 2 {
		t.Fatalf("service count = %d, want 2 runnable services", dep.ServiceCount)
	}

	// The whole point of the count: it is what lets the deployment finish.
	for _, svc := range []string{"db", "web"} {
		if err := st.AdvanceDeploymentService(ctx, server, res.ID, svc, "rollout", true, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GetDeployment(ctx, orgID, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" {
		t.Fatalf("every service succeeded but the deployment is %q", got.Status)
	}
}

// A cluster workload has no server of its own, so its nodes must be able to
// report on it and its build server must be able to render the build.
func TestClusterDeploymentBuildsAndReports(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cluster_build"
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
	buildServer := connectServer(t, st, orgID, "builder")

	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: projectID, Provider: "github", RepoFullName: "acme/app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ClusterID: cluster.ID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: envID, ConnectionID: conn.ID,
		Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, dep.ID, buildServer); err != nil {
		t.Fatal(err)
	}

	// The build server picks the workload up even though it is in no cluster and
	// the resource is on no server.
	specs, err := st.ClusterBuildSpecsForServer(ctx, buildServer)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].ResourceID != app.ID {
		t.Fatalf("cluster builds for the build server = %+v", specs)
	}
	if other, _ := st.ClusterBuildSpecsForServer(ctx, cpServer); len(other) != 0 {
		t.Fatalf("a node that is not the build server must render no builds: %+v", other)
	}

	// And the pipeline advances: build server for the build, control-plane node
	// for the rollout it applies.
	if err := st.AdvanceDeploymentForResource(ctx, buildServer, app.ID, "build", true, "", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(ctx, orgID, dep.ID); got.Status != "deploying" {
		t.Fatalf("after the build, status = %q", got.Status)
	}
	if err := st.AdvanceDeploymentForResource(ctx, cpServer, app.ID, "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDeployment(ctx, orgID, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" {
		t.Fatalf("a cluster node must be able to complete its own deployment, status = %q", got.Status)
	}

	// The build drops out of the build server's document once the deployment is
	// no longer in flight, rather than sitting there forever.
	if after, _ := st.ClusterBuildSpecsForServer(ctx, buildServer); len(after) != 0 {
		t.Fatalf("a finished build still renders: %+v", after)
	}

	// The control-plane node holds its workload back until the build reports, so
	// it has to be re-rendered when that happens. Without the nudge the deploy
	// stalls until the next 60-second resync, once per stage.
	peers, err := st.DeployPeersForResource(ctx, app.ID, buildServer)
	if err != nil {
		t.Fatal(err)
	}
	nudged := map[string]bool{}
	for _, p := range peers {
		nudged[p.ServerID] = true
		if p.OrgID != orgID {
			t.Fatalf("peer %s carries org %q", p.ServerID, p.OrgID)
		}
	}
	if !nudged[cpServer] || !nudged[worker] {
		t.Fatalf("cluster nodes must be nudged when the build reports: %+v", peers)
	}
	if nudged[buildServer] {
		t.Fatal("the reporting server has just been rendered and must not be nudged again")
	}
}

// TestDeployLogsAcceptedFromEveryServerThatRunsTheDeploy pins the last member of
// the render/report asymmetry family, and the one that hurts most in practice:
// the logs. A deploy log line is written by whichever server executed the op —
// the build server for the build output, a cluster node or a Compose placement
// host for the rollout and its startup logs. Matching those writes against the
// deploy TARGET alone silently discarded every one of them, so the deploy view
// for a build-server or cluster deploy showed nothing at all: a failure with no
// output is the one thing an operator cannot act on (SIGMA-181).
func TestDeployLogsAcceptedFromEveryServerThatRunsTheDeploy(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_deploylogs"

	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	runServer := connectServer(t, st, orgID, "run")
	buildServer := connectServer(t, st, orgID, "build")
	placedHost := connectServer(t, st, orgID, "placed")
	for _, id := range []string{runServer, buildServer, placedHost} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}

	// A Compose app whose 'db' service is placed on a third host.
	spec := fmt.Sprintf(`{"compose":{"services":[{"name":"web","build":"."},{"name":"db","image":"postgres:16","serverId":%q}]}}`, placedHost)
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: runServer, Name: "api", Kind: "app",
		Spec: json.RawMessage(spec),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: runServer,
		Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, dep.ID, buildServer); err != nil {
		t.Fatal(err)
	}

	for _, w := range []struct {
		server, stream, line string
	}{
		{runServer, "startup", "listening on :8080"},
		{buildServer, "build", "step 2/7 : COPY go.mod ."},
		{placedHost, "startup", "FATAL: database files are incompatible"},
	} {
		if err := st.AppendDeployLog(ctx, w.server, dep.ID, w.stream, w.line); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := st.DeployLogsSince(ctx, dep.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := ""
	for _, l := range logs {
		seen += l.Stream + ":" + l.Line + "\n"
	}
	if len(logs) != 3 {
		t.Fatalf("deploy logs = %+v, want one line from each of the three servers that ran part of this deploy", logs)
	}
	if !strings.Contains(seen, "build:step 2/7 : COPY go.mod .") {
		t.Errorf("the build server's output was dropped — a build-server deploy shows an empty log:\n%s", seen)
	}
	if !strings.Contains(seen, "startup:FATAL: database files are incompatible") {
		t.Errorf("the placement host's startup logs were dropped — the crash reason is lost:\n%s", seen)
	}

	// The guard still holds: a server with no part in this deployment cannot
	// write into its log, forged stream or not.
	other := connectServer(t, st, orgID, "elsewhere")
	if err := st.AppendDeployLog(ctx, other, dep.ID, "build", "injected"); err != nil {
		t.Fatal(err)
	}
	after, _ := st.DeployLogsSince(ctx, dep.ID, 0, 100)
	if len(after) != 3 {
		t.Fatalf("an unrelated server wrote into another deployment's log: %+v", after)
	}
}

// TestBuildServerCarriesIntoRedeployRollbackAndConfig is SIGMA-231. Only the
// push path and CreateHeadDeployment ever wrote build_server_id, so pressing
// Redeploy on a cluster app minted a row with a NULL build server —
// ClusterBuildSpecsForServer, the only thing that puts a cluster workload's
// clone+build ops into ANY document, then matched nothing and the deployment sat
// queued until TimeoutStaleDeployments failed it 45 minutes later with a message
// blaming the agent. Rollback and config deploys drop the column the same way,
// and because each of these copies from the MOST RECENT row, one drop poisons
// every redeploy after it.
func TestBuildServerCarriesIntoRedeployRollbackAndConfig(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_buildserver_carry"
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
	builder := connectServer(t, st, orgID, "builder")

	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: projectID, Provider: "github", RepoFullName: "acme/app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ClusterID: cluster.ID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	buildServerOf := func(depID string) string {
		t.Helper()
		var got string
		if err := st.Pool.QueryRow(ctx,
			`SELECT COALESCE(build_server_id,'') FROM deployments WHERE id = $1`, depID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The first deploy is the push path's: it records the build server and ships.
	first, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: envID, ConnectionID: conn.ID,
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "abc1234567", ConfigHash: "cfg1",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, first.ID, builder); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentStatus(ctx, first.ID, store.DeploymentStatusUpdate{
		Status: "success", ImageDigest: "sha256:abc", MarkFinished: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Redeploy. Without the build server this row can be built by nobody.
	red, _, err := st.CreateManualRedeploy(ctx, orgID, app.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if got := buildServerOf(red.ID); got != builder {
		t.Fatalf("redeploy build_server_id = %q, want %q", got, builder)
	}
	specs, err := st.ClusterBuildSpecsForServer(ctx, builder)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].ResourceID != app.ID {
		t.Fatalf("nothing renders clone+build for the redeployed cluster app: %+v", specs)
	}
	tgt, err := st.DeployTargetForResource(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.DeploymentID != red.ID || tgt.BuildServerID != builder {
		t.Fatalf("deploy target = %s/%q, want the redeploy on %q", tgt.DeploymentID, tgt.BuildServerID, builder)
	}

	// Rollback. It re-ships a retained image so it renders no build, but it
	// becomes the newest row — and the next redeploy copies from the newest row,
	// so dropping the column here loses the build server for good.
	if err := st.SetDeploymentStatus(ctx, red.ID, store.DeploymentStatusUpdate{
		Status: "failed", Detail: "build failed", MarkFinished: true,
	}); err != nil {
		t.Fatal(err)
	}
	rb, _, err := st.CreateRollback(ctx, orgID, app.ID, first.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if got := buildServerOf(rb.ID); got != builder {
		t.Fatalf("rollback build_server_id = %q, want %q", got, builder)
	}

	// Config deploy (domain attached / secret changed) off a successful release.
	if err := st.SetDeploymentStatus(ctx, rb.ID, store.DeploymentStatusUpdate{
		Status: "success", MarkFinished: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConfigDeployments(ctx, orgID, []string{app.ID}, "operator", "domain attached"); err != nil {
		t.Fatal(err)
	}
	var cfgDep string
	if err := st.Pool.QueryRow(ctx,
		`SELECT id FROM deployments WHERE org_id = $1 AND resource_id = $2 AND trigger = 'config'
		  ORDER BY created_at DESC LIMIT 1`, orgID, app.ID).Scan(&cfgDep); err != nil {
		t.Fatal(err)
	}
	if got := buildServerOf(cfgDep); got != builder {
		t.Fatalf("config deploy build_server_id = %q, want %q", got, builder)
	}

	// And the chain holds: a redeploy after all that still knows where to build.
	if err := st.SetDeploymentStatus(ctx, cfgDep, store.DeploymentStatusUpdate{
		Status: "success", MarkFinished: true,
	}); err != nil {
		t.Fatal(err)
	}
	again, _, err := st.CreateManualRedeploy(ctx, orgID, app.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if got := buildServerOf(again.ID); got != builder {
		t.Fatalf("redeploy after a rollback+config chain lost the build server: %q", got)
	}
}

// TestSecondClusterDeploySupersedesTheFirst is SIGMA-232. supersedeInFlightTx
// short-circuits on an empty server id, and a cluster workload HAS no server —
// its deployments carry server_id NULL. So the at-most-one-in-flight-per-resource
// invariant that the whole op-status advance path is documented to rely on never
// held for cluster apps: two pushes a few minutes apart left the first row in
// flight forever, every later op status advanced the second, and 45 minutes on
// TimeoutStaleDeployments failed the abandoned row — paging the operator about a
// deploy that was correctly replaced and whose successor shipped fine.
func TestSecondClusterDeploySupersedesTheFirst(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cluster_supersede"
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
	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: projectID, Provider: "github", RepoFullName: "acme/app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ClusterID: cluster.ID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// A server-bound app in the same environment: the push deploys both, and the
	// server path's supersede must keep working exactly as before.
	hostApp, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: worker, Name: "web", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":3000}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	push := func(id, sha string) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO deploy_requests (id, org_id, connection_id, environment_id, kind, ref, sha, branch, status)
			VALUES ($1,$2,$3,$4,'deploy','refs/heads/main',$5,'main','queued')`,
			id, orgID, conn.ID, envID, sha); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DrainDeployRequests(ctx); err != nil {
			t.Fatal(err)
		}
	}
	statuses := func(resID string) map[string]int {
		t.Helper()
		rows, err := st.Pool.Query(ctx,
			`SELECT status, count(*) FROM deployments WHERE org_id = $1 AND resource_id = $2 GROUP BY status`,
			orgID, resID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var s string
			var n int
			if err := rows.Scan(&s, &n); err != nil {
				t.Fatal(err)
			}
			out[s] = n
		}
		return out
	}
	inFlight := func(resID string) int {
		s := statuses(resID)
		return s["queued"] + s["building"] + s["deploying"]
	}

	push("dpr_one", "aaaaaaa1111")
	if n := inFlight(app.ID); n != 1 {
		t.Fatalf("first push left %d in-flight cluster deployments, want 1", n)
	}
	push("dpr_two", "bbbbbbb2222")

	if n := inFlight(app.ID); n != 1 {
		t.Fatalf("a second push left %d in-flight cluster deployments — the first is an orphan "+
			"that TimeoutStaleDeployments will fail and alert on: %+v", n, statuses(app.ID))
	}
	if got := statuses(app.ID)["superseded"]; got != 1 {
		t.Fatalf("the replaced cluster deployment is not marked superseded: %+v", statuses(app.ID))
	}
	// The server-bound sibling is the control: its supersede already worked and
	// must keep working through the same code path.
	if n := inFlight(hostApp.ID); n != 1 {
		t.Fatalf("server-bound app has %d in-flight deployments: %+v", n, statuses(hostApp.ID))
	}
	if got := statuses(hostApp.ID)["superseded"]; got != 1 {
		t.Fatalf("server-bound supersede regressed: %+v", statuses(hostApp.ID))
	}

	// The superseded row is terminal, so the sweeper never touches it and no
	// deploy_failed alert is ever enqueued for it. Age every row of this resource
	// past the timeout: only the one still in flight may be failed.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET created_at = now() - interval '2 hours' WHERE resource_id = $1`,
		app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TimeoutStaleDeployments(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := statuses(app.ID)["failed"]; got != 1 {
		// Only the still-in-flight successor may be timed out here.
		t.Fatalf("stale sweep failed %d cluster deployments, want only the in-flight one: %+v",
			got, statuses(app.ID))
	}
}

// SIGMA-258: the agent registry-credential release is scoped to servers that
// actually build or pull for the org.
//
// The credential is the org's registry username and password — in practice a
// ghcr.io or Docker Hub PAT with PUSH rights to every image the org publishes.
// The endpoint authenticated the agent token and then handed that credential to
// ANY server in the org, because the query behind it filtered on org_id alone
// and used the server id only to label the audit row. One compromised host —
// a staging box with a contractor's shell on it — could read ~/.sigmad/state,
// call this endpoint, and overwrite :latest for every image the org publishes;
// the next deploy or restart anywhere in the fleet pulls the poisoned tag.
//
// Need is the gate now: the builder of an in-flight deployment (it has to push),
// the host that deployment rolls out on (it has to pull what the builder
// pushed), and a node of a cluster carrying workloads (same pull). Everybody
// else gets the same 404 as an org with no registry at all.
func TestAgentRegistryCredential_ForeignServerDenied(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.New(log, st, st, st, api.Options{
		DevServiceToken: "dev",
		Registry:        st,
		DSDPublicKey:    dsdKey.Public().(ed25519.PublicKey),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	orgID := "org_reg_scope"
	if _, err := st.SetImageRegistry(ctx, orgID, store.SetImageRegistryInput{
		Host: "ghcr.io", Namespace: "acme", Username: "bot", Password: "push-token",
	}, "admin"); err != nil {
		t.Fatal(err)
	}

	// Enrolling by hand rather than through connectServer: this test acts as the
	// agent over HTTP, so it needs the agent TOKEN, not just the server id.
	enroll := func(name string) (id, token string) {
		t.Helper()
		bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, "general", "", "", "admin", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := st.RegisterServer(ctx, bootTok, name, "0.1.0", json.RawMessage(`{}`), "")
		if err != nil {
			t.Fatal(err)
		}
		return reg.Server.ID, reg.AgentToken
	}
	runServer, runToken := enroll("run")
	buildServer, buildToken := enroll("build")
	strangerID, strangerToken := enroll("staging-contractor-box")

	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{runServer, buildServer, strangerID} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: runServer, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: runServer,
		ConnectionID: conn.ID, Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, dep.ID, buildServer); err != nil {
		t.Fatal(err)
	}

	fetch := func(token string) (int, store.RegistryCredential) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/v1/agent/registry-credential", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var cred store.RegistryCredential
		_ = json.NewDecoder(resp.Body).Decode(&cred)
		return resp.StatusCode, cred
	}

	// The builder must still get it: it is the machine that runs docker push.
	if code, cred := fetch(buildToken); code != http.StatusOK || cred.Password != "push-token" {
		t.Fatalf("build server → %d %+v, want 200 with the credential", code, cred)
	}
	// So must the deploy target: the image it rolls out lives in a private
	// registry and an anonymous pull is a 401 (SIGMA-243).
	if code, cred := fetch(runToken); code != http.StatusOK || cred.Password != "push-token" {
		t.Fatalf("deploy target → %d %+v, want 200 with the credential", code, cred)
	}
	// The stranger neither builds nor pulls anything for this org.
	code, cred := fetch(strangerToken)
	if code != http.StatusNotFound {
		t.Fatalf("uninvolved server → %d, want 404", code)
	}
	if cred.Password != "" || cred.Username != "" {
		t.Fatalf("uninvolved server was handed the registry credential: %+v", cred)
	}

	// The refusal is still audited — a host asking for a credential it has no
	// business holding is exactly the line an incident is reconstructed from.
	entries, err := st.ListAudit(ctx, orgID, 50)
	if err != nil {
		t.Fatal(err)
	}
	denied := false
	for _, e := range entries {
		if strings.Contains(e.Action, "Registry credential") && strings.Contains(e.Actor, strangerID) {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("the refused release left no audit trail: %+v", entries)
	}
}
