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
	"encoding/json"
	"errors"
	"testing"

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
	serverID := connectServer(t, st, orgID, "builder")
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
