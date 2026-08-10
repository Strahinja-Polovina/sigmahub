package integration

// Clusters and GPU model hosting against a real database.
//
// These paths ship a lot of hand-written SQL (a new table each, a weighted port
// allocator shared with databases and object storage, a FOR UPDATE role check).
// None of it is exercised by a pure-Go test, and a NOT NULL or column-name
// mistake in that SQL only shows up when it runs — which is exactly how the
// forced-disconnect bug reached CI.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// clusterFixture builds an org with a project, an environment and two servers
// attached to it, returning the environment and server ids.
func clusterFixture(t *testing.T, st *store.Store, orgID string) (envID, cpServer, worker string) {
	t.Helper()
	ctx := context.Background()

	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	cpServer = connectServer(t, st, orgID, "node-a")
	worker = connectServer(t, st, orgID, "node-b")
	for _, id := range []string{cpServer, worker} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	return env.ID, cpServer, worker
}

func TestClusterLifecycle(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cluster"
	envID, cpServer, worker := clusterFixture(t, st, orgID)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID:  envID,
		Name:           "production",
		ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if cluster.Status != "provisioning" {
		t.Fatalf("new cluster status = %q, want provisioning", cluster.Status)
	}

	// One cluster per environment keeps "deploy to the cluster" unambiguous.
	if _, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "second", ControlPlaneID: worker,
	}, "admin"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second cluster in the same environment err = %v, want ErrConflict", err)
	}

	if err := st.AddClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListClusters(ctx, orgID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || len(list[0].Nodes) != 2 {
		t.Fatalf("clusters = %+v", list)
	}
	roles := map[string]string{}
	for _, n := range list[0].Nodes {
		roles[n.ServerID] = n.Role
		if n.ServerName == "" {
			t.Fatalf("node is missing its server name: %+v", n)
		}
	}
	if roles[cpServer] != store.NodeRoleControlPlane || roles[worker] != store.NodeRoleWorker {
		t.Fatalf("roles = %+v", roles)
	}

	// The reconciler's view: the join token unwraps, and a worker learns where
	// to dial. Without this the k8s.node op can only ever fail.
	cpm, ok, err := st.ClusterMembershipForServer(ctx, cpServer)
	if err != nil || !ok {
		t.Fatalf("control-plane membership: ok=%v err=%v", ok, err)
	}
	if cpm.JoinToken == "" {
		t.Fatal("join token did not unwrap")
	}
	wm, ok, err := st.ClusterMembershipForServer(ctx, worker)
	if err != nil || !ok {
		t.Fatalf("worker membership: ok=%v err=%v", ok, err)
	}
	if wm.JoinToken != cpm.JoinToken {
		t.Fatal("both nodes must present the same cluster token")
	}
	if wm.ControlPlaneMeshIP == "" {
		t.Fatal("a worker with no control-plane address has nothing to join")
	}

	// The control-plane node cannot be removed — that destroys the cluster and
	// must be an explicit delete.
	if err := st.RemoveClusterNode(ctx, orgID, cluster.ID, cpServer, "admin"); !errors.Is(err, store.ErrControlPlaneNode) {
		t.Fatalf("removing the control plane err = %v, want ErrControlPlaneNode", err)
	}
	if err := st.RemoveClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}

	// Deleting returns the servers to re-render so k3s is torn down on each.
	servers, err := st.DeleteCluster(ctx, orgID, cluster.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0] != cpServer {
		t.Fatalf("servers to re-render = %+v, want just the control plane", servers)
	}
	if after, _ := st.ListClusters(ctx, orgID, ""); len(after) != 0 {
		t.Fatalf("cluster survived delete: %+v", after)
	}
}

// SIGMA-312: deleting a cluster-deployed resource — or the whole cluster — must
// queue the teardown of its Kubernetes manifests on the control-plane node.
//
// A cluster workload's manifests were only ever pruned as a side effect of
// APPLYING that workload, so a deletion (which stops the resource being rendered
// at all) left the Deployment, Service and Ingress running in k3s forever:
// answering on the attached domain, consuming node CPU/RAM and holding the org's
// registry pull secret, with nothing left in the product describing them.
// DeleteResource did not even name a server to re-render, because a cluster
// resource has no server_id of its own.
func TestDeletedClusterWorkloadsAreTornDown(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cluster_teardown"
	envID, cpServer, _ := clusterFixture(t, st, orgID)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	mkApp := func(name string, spec json.RawMessage) store.Resource {
		t.Helper()
		res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
			EnvironmentID: envID, ClusterID: cluster.ID, Name: name, Kind: "app", Spec: spec,
		}, "admin")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// A Compose app is N workloads, so the teardown has to name all of them.
	app := mkApp("api", json.RawMessage(`{"compose":{"services":[
		{"name":"web","image":"nginx:1.27","ports":[80]},
		{"name":"worker","image":"busybox:1"}]}}`))

	// ── Deleting one resource ────────────────────────────────────────────────
	serverID, err := st.DeleteResource(ctx, orgID, app.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if serverID != cpServer {
		t.Fatalf("DeleteResource returned server %q, want the cluster's control plane %q — "+
			"nothing nudges the reconciler otherwise", serverID, cpServer)
	}
	pending, err := st.PendingDestructiveOpsForServer(ctx, orgID, cpServer)
	if err != nil {
		t.Fatal(err)
	}
	var target string
	for _, p := range pending {
		if p.OpKind == dsd.KindK8sRemove {
			target = p.Target
		}
	}
	if target == "" {
		t.Fatalf("no k8s teardown queued for the deleted cluster resource: %+v", pending)
	}
	for _, svc := range []string{"web", "worker"} {
		if want := dsd.K8sWorkloadName(app.ID, svc); !strings.Contains(target, want) {
			t.Fatalf("teardown target %q does not name workload %q", target, want)
		}
	}

	// ── Deleting the whole cluster ───────────────────────────────────────────
	//
	// The cluster goes AFTER its workloads. Deleting it first would leave their
	// manifests on a node the product has forgotten, with the cluster_nodes rows
	// (and therefore the only address a teardown could be sent to) cascaded away.
	// It could not even succeed: a resource must name exactly one target, so the
	// ON DELETE SET NULL this once promised violated resources_one_target and the
	// delete failed with a check-constraint error and no explanation.
	survivor := mkApp("site", json.RawMessage(`{"image":"nginx:1.27","ports":[{"container":80}]}`))
	_, err = st.DeleteCluster(ctx, orgID, cluster.ID, "admin")
	var stillRunning store.ErrClusterWorkloads
	if !errors.As(err, &stillRunning) {
		t.Fatalf("deleting a cluster that still runs workloads err = %v, want ErrClusterWorkloads", err)
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("the refusal must be a 409: %v", err)
	}
	if len(stillRunning.Names) != 1 || stillRunning.Names[0] != "site" {
		t.Fatalf("the refusal must name what is in the way: %+v", stillRunning.Names)
	}

	// Delete the workload — which queues ITS teardown — and the cluster goes.
	if _, err := st.DeleteResource(ctx, orgID, survivor.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	pending, err = st.PendingDestructiveOpsForServer(ctx, orgID, cpServer)
	if err != nil {
		t.Fatal(err)
	}
	want := dsd.K8sWorkloadName(survivor.ID, "")
	found := false
	for _, p := range pending {
		if p.OpKind == dsd.KindK8sRemove && strings.Contains(p.Target, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no teardown queued for workload %q: %+v", want, pending)
	}
	if _, err := st.DeleteCluster(ctx, orgID, cluster.ID, "admin"); err != nil {
		t.Fatalf("an emptied cluster must delete: %v", err)
	}
}

// A cluster-deployed app has no server of its own, and a stateful kind is
// refused outright.
func TestClusterResourcePlacement(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cluster_res"
	envID, cpServer, _ := clusterFixture(t, st, orgID)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID,
		ClusterID:     cluster.ID,
		Name:          "api",
		Kind:          "app",
		Spec:          json.RawMessage(`{"image":"nginx:1.27","ports":[{"container":80}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if app.ServerID != "" {
		t.Fatalf("a cluster workload must not be pinned to a server: %q", app.ServerID)
	}

	// The reconciler reads cluster workloads separately from server resources.
	specs, err := st.ResourceSpecsForCluster(ctx, cluster.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].ResourceID != app.ID {
		t.Fatalf("cluster workloads = %+v", specs)
	}

	// A database aimed at the cluster is refused: rescheduling it onto a node
	// without its data is data loss, not a slow deploy.
	_, err = st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ClusterID: cluster.ID, Name: "db", Kind: "postgres",
	}, "admin")
	var notClusterable store.ErrKindNotClusterable
	if !errors.As(err, &notClusterable) {
		t.Fatalf("postgres into a cluster err = %v, want ErrKindNotClusterable", err)
	}

	// Targeting both a server and a cluster is incoherent and must be refused
	// rather than silently preferring one.
	_, err = st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ClusterID: cluster.ID, ServerID: cpServer,
		Name: "both", Kind: "app",
	}, "admin")
	if err == nil {
		t.Fatal("a resource targeting a server AND a cluster must be refused")
	}
}

// GPU model hosting: provisioning allocates a mesh port that cannot collide
// with a database or object-store port on the same host, and the endpoint
// readout renders from the runtime catalog.
func TestLLMEndpointProvisioning(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_llm"

	proj, err := st.CreateProject(ctx, orgID, "ai", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	gpu := connectTypedServer(t, st, orgID, "gpu-1", "gpu")
	if err := st.AttachServer(ctx, orgID, env.ID, gpu, "admin"); err != nil {
		t.Fatal(err)
	}

	llm, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID,
		ServerID:      gpu,
		Name:          "llama",
		Kind:          "llm",
		Spec:          json.RawMessage(`{"engine":"vllm","model":"meta-llama/Llama-3.1-8B"}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	targets, err := st.LLMTargetsForServer(ctx, gpu)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := targets[llm.ID]
	if !ok {
		t.Fatalf("no inference target rendered for %s: %+v", llm.ID, targets)
	}
	if target.Engine != "vllm" || target.Model != "meta-llama/Llama-3.1-8B" {
		t.Fatalf("target = %+v", target)
	}
	if target.Port == 0 {
		t.Fatal("no mesh port allocated — the endpoint would be unrenderable")
	}

	info, err := st.GetLLM(ctx, orgID, llm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Image == "" {
		t.Fatal("endpoint readout lost its runtime image")
	}
	if info.Endpoint == "" {
		t.Fatalf("no endpoint URL rendered (mesh ip %q, port %d)", info.Host, info.Port)
	}

	// A second inference endpoint on the same host must get a distinct port —
	// the allocator scans llm_endpoints alongside the database and S3 tables.
	second, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: gpu, Name: "mistral", Kind: "llm",
		Spec: json.RawMessage(`{"engine":"vllm","model":"mistralai/Mistral-7B"}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	targets, _ = st.LLMTargetsForServer(ctx, gpu)
	if targets[second.ID].Port == target.Port {
		t.Fatalf("two endpoints collided on port %d", target.Port)
	}

	// An unknown runtime fails the create loudly rather than provisioning a
	// resource nothing can render.
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: gpu, Name: "bogus", Kind: "llm",
		Spec: json.RawMessage(`{"engine":"not-a-runtime"}`),
	}, "admin"); err == nil {
		t.Fatal("an unknown inference runtime must be refused")
	}
}

// SIGMA-331: the write side of "where does this resource run" had two answers
// and the read side had one. CreateResource inserted cluster_id but never
// selected it back, and ListResources never selected it at all, so a workload
// deployed into a cluster came back over the API with an empty ServerID and no
// ClusterID either — the dashboard rendered a running app as if it ran nowhere.
// Round-trip the field through both paths.
func TestResourceClusterIDRoundTrip(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cluster_roundtrip"
	envID, cpServer, _ := clusterFixture(t, st, orgID)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID,
		ClusterID:     cluster.ID,
		Name:          "api",
		Kind:          "app",
		Spec:          json.RawMessage(`{"image":"nginx:1.27","ports":[{"container":80}]}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if app.ClusterID != cluster.ID {
		t.Fatalf("CreateResource returned ClusterID = %q, want %q", app.ClusterID, cluster.ID)
	}

	list, err := st.ListResources(ctx, orgID, envID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("resources = %+v", list)
	}
	if list[0].ClusterID != cluster.ID {
		t.Fatalf("ListResources returned ClusterID = %q, want %q", list[0].ClusterID, cluster.ID)
	}

	// A server-bound resource keeps an empty ClusterID: the two targets are
	// mutually exclusive and neither may leak the other's id.
	srv, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: cpServer, Name: "pinned", Kind: "postgres",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if srv.ClusterID != "" {
		t.Fatalf("server-bound resource ClusterID = %q, want empty", srv.ClusterID)
	}
	after, err := st.ListResources(ctx, orgID, envID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range after {
		if r.ID == srv.ID && r.ClusterID != "" {
			t.Fatalf("listed server-bound resource ClusterID = %q, want empty", r.ClusterID)
		}
	}
}
