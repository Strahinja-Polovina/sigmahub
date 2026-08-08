package integration

// The fleet e2e: a real control plane, real sigmad agents as separate
// processes, and a real Docker daemon.
//
// Everything else in this package stops at the CP boundary — it speaks the
// agent's HTTP protocol from the test itself. That leaves the most expensive
// class of bug completely uncovered: the ones that only exist BETWEEN the two
// halves. The multi-server Compose deadlock was exactly that shape. Rendering
// placed a service's ops in its host's document and the store refused that
// host's status report, so the service ran, reported, and the deployment sat in
// 'queued' forever. Both halves passed their own tests.
//
// So this runs the actual binary. sigmad is built from the agent module,
// launched twice with its own data directory each, enrolled against the real
// CP over HTTP, and left to converge on its own. Nothing here simulates an
// agent.
//
// Runs when SIGMAHUB_FLEET_E2E=1, CP_TEST_DATABASE_URL points at a database and
// Docker is reachable. The images are built locally FROM scratch so the test
// needs no registry.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// idleImage is a container that starts and stays up, built with no base layer
// so nothing has to be pulled. A Compose service with no ports gets a "none"
// health probe, so "running" is the readiness signal — which is all this needs
// to prove the rollout completed.
const idleImage = "sigmahub-e2e/idle:1"

const idleSource = `package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}
`

func fleetGate(t *testing.T) {
	t.Helper()
	if os.Getenv("SIGMAHUB_FLEET_E2E") == "" {
		t.Skip("set SIGMAHUB_FLEET_E2E=1 to run the fleet e2e (needs Docker and a database)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable: %s", out)
	}
}

// buildIdleImage compiles the idle binary statically and packages it with no
// base layer.
func buildIdleImage(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("docker", "image", "inspect", idleImage).CombinedOutput(); err == nil {
		return // already built by an earlier run
	} else if len(out) == 0 {
		t.Fatal("docker image inspect produced nothing")
	}

	dir := t.TempDir()
	write(t, filepath.Join(dir, "main.go"), idleSource)
	write(t, filepath.Join(dir, "go.mod"), "module idle\n\ngo 1.21\n")
	build := exec.Command(goBinary(t), "build", "-o", "idle", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build idle binary: %v\n%s", err, out)
	}
	write(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\nCOPY idle /idle\nENTRYPOINT [\"/idle\"]\n")
	if out, err := exec.Command("docker", "build", "-t", idleImage, dir).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", idleImage, err, out)
	}
}

func goBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("SIGMAHUB_GO"); p != "" {
		return p
	}
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}
	return p
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildSigmad compiles the real agent. Building it here rather than pointing at
// a prebuilt binary is the point: the test always runs the agent that matches
// the tree under test.
func buildSigmad(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "sigmad")
	cmd := exec.Command(goBinary(t), "build", "-o", out, "./cmd/sigmad")
	cmd.Dir = "../../../agent"
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sigmad: %v\n%s", err, b)
	}
	return out
}

// fleetAgent is one running sigmad process.
type fleetAgent struct {
	name     string
	serverID string
	cmd      *exec.Cmd
	logPath  string
}

// startAgent enrolls and runs a real sigmad against the CP.
func startAgent(t *testing.T, st *store.Store, binary, cpURL, orgID, name string) *fleetAgent {
	t.Helper()
	ctx := context.Background()
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	logPath := filepath.Join(dataDir, "sigmad.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary,
		"-endpoint", cpURL,
		"-bootstrap-token", bootTok,
		"-data-dir", dataDir,
		"-name", name,
		// Short so a missed reconcile nudge still converges inside the test's
		// patience rather than after the production 30s.
		"-interval", "2s",
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &fleetAgent{name: name, cmd: cmd, logPath: logPath}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = logFile.Close()
	})

	// The agent registers itself; wait for the server row to appear.
	waitFor(t, 45*time.Second, "agent "+name+" to enroll", func() bool {
		servers, lerr := st.ListServers(ctx, orgID)
		if lerr != nil {
			return false
		}
		for _, s := range servers {
			if s.Name == name {
				a.serverID = s.ID
				return true
			}
		}
		return false
	}, a.tail)
	return a
}

// tail returns the end of the agent's log, for a failure message that explains
// itself instead of just saying a deadline passed.
func (a *fleetAgent) tail() string {
	b, err := os.ReadFile(a.logPath)
	if err != nil {
		return "(no log)"
	}
	// Heartbeats are one line every couple of seconds and would push the lines
	// that explain a failure out of any reasonable tail.
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.Contains(line, "heartbeat ok") || strings.Contains(line, "mesh: peer config") {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) > 30 {
		kept = kept[len(kept)-30:]
	}
	return a.name + " log:\n" + strings.Join(kept, "\n")
}

// waitFor polls until cond holds, failing with whatever context the callers
// supply. Polling rather than sleeping keeps the suite fast when things work.
func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool, context ...func() string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	var extra []string
	for _, c := range context {
		extra = append(extra, c())
	}
	t.Fatalf("timed out after %s waiting for %s\n%s", limit, what, strings.Join(extra, "\n\n"))
}

// dockerState lists every container the fleet created, running or not, with why
// it stopped. "the container never came up" is not a diagnosis; the exit status
// and the last lines of its output are.
func dockerState(prefix string) func() string {
	return func() string {
		out, err := exec.Command("docker", "ps", "-a", "--filter", "name="+prefix,
			"--format", "{{.Names}}\t{{.Status}}\t{{.Image}}").CombinedOutput()
		if err != nil {
			return "docker ps failed: " + string(out)
		}
		report := "docker containers matching " + prefix + ":\n" + strings.TrimSpace(string(out))
		names, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}").Output()
		for _, n := range strings.Fields(string(names)) {
			logs, _ := exec.Command("docker", "logs", "--tail", "10", n).CombinedOutput()
			if len(strings.TrimSpace(string(logs))) > 0 {
				report += "\n--- " + n + " ---\n" + strings.TrimSpace(string(logs))
			}
		}
		return report
	}
}

// deploymentState reports what the control plane believes, which is the other
// half of any disagreement between "a container is running" and "the deploy
// finished".
func deploymentState(st *store.Store, orgID, depID string) func() string {
	return func() string {
		got, err := st.GetDeployment(context.Background(), orgID, depID)
		if err != nil {
			return "deployment unreadable: " + err.Error()
		}
		var dsdVersion int64
		_ = st.Pool.QueryRow(context.Background(),
			`SELECT COALESCE(dsd_version,0) FROM deployments WHERE id = $1`, depID).Scan(&dsdVersion)
		return fmt.Sprintf("deployment %s: status=%s services=%v serviceCount=%d stampedDSDVersion=%d",
			depID, got.Status, got.ServiceStatus, got.ServiceCount, dsdVersion)
	}
}

func containerRunning(name string) bool {
	out, err := exec.Command("docker", "ps", "--filter", "name="+name, "--format", "{{.Names}}").CombinedOutput()
	return err == nil && strings.Contains(string(out), name)
}

// removeContainers cleans up whatever the agents created, so a rerun starts
// from a clean host.
func removeContainers(prefix string) {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "name="+prefix).CombinedOutput()
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		_ = exec.Command("docker", "rm", "-f", id).Run()
	}
}

// fleetCP stands up the real control plane in-process behind a real HTTP
// server, which is what the agents dial.
func fleetCP(t *testing.T, st *store.Store, dsdKey ed25519.PrivateKey) (*httptest.Server, *reconciler.Reconciler, func() string) {
	t.Helper()
	// The control plane's own log is a diagnostic, not noise: a render that
	// fails happens in a background goroutine, so discarding it turns "the
	// service never started" into a timeout with no cause attached.
	logPath := filepath.Join(t.TempDir(), "cp.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	log := slog.New(slog.NewTextHandler(logFile, nil))
	rec := reconciler.New(log, st, dsdKey)
	api.SetLongPollTimeout(2 * time.Second)
	srv := api.New(log, st, st, st, api.Options{
		DevServiceToken: "dev",
		DSDStore:        st,
		DSDWaiter:       rec,
		Reconcile:       rec,
		Clusters:        st,
		Registry:        st,
		LLM:             st,
		DNS:             st,
		DSDPublicKey:    dsdKey.Public().(ed25519.PublicKey),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, rec, func() string { return "control plane log:\n" + tailFile(logPath, 30) }
}

// tailFile returns the last n lines, for failure messages that carry a cause.
func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// A Compose app whose services live on DIFFERENT servers.
//
// This is the shape that was broken end to end. `web` depends on `db`, `db` is
// placed on the second server, and the control plane holds `web` back until
// `db` reports success — a gate that only works if the second server's status
// report is accepted. It was not: advancement was scoped to the deployment's
// own server_id, so the report was dropped, `db` never counted as done, and
// `web` never rendered. The deployment stayed 'queued' with a container
// happily running next to it.
func TestFleetMultiServerComposeDeploy(t *testing.T) {
	fleetGate(t)
	st, dsdKey := testStore(t)
	ctx := context.Background()
	// Published to a registry rather than left as a local tag: the deploy
	// pipeline pulls, which is the path every cross-host image takes.
	image := publishIdleImage(t)
	sigmad := buildSigmad(t)

	ts, rec, cpLog := fleetCP(t, st, dsdKey)
	orgID := "org_fleet"

	proj, err := st.CreateProject(ctx, orgID, "shop", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "test")
	if err != nil {
		t.Fatal(err)
	}

	appHost := startAgent(t, st, sigmad, ts.URL, orgID, "fleet-app")
	dataHost := startAgent(t, st, sigmad, ts.URL, orgID, "fleet-data")
	for _, a := range []*fleetAgent{appHost, dataHost} {
		if err := st.AttachServer(ctx, orgID, env.ID, a.serverID, "test"); err != nil {
			t.Fatal(err)
		}
	}

	// A secret the placed service must inject. The reference is rendered into
	// one host's document and the value comes back through a different endpoint,
	// so a mismatch between the two only shows up here: the container fails to
	// create and the service never starts.
	if _, err := st.CreateSecret(ctx, orgID, "test", store.CreateSecretInput{
		ProjectID: proj.ID, EnvironmentID: env.ID, Name: "DATABASE_URL",
		Value: "postgres://u:p@db/app", EnvVar: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Two prebuilt services, one per host. Nothing builds from source, so no git
	// access is involved — the deployment exists to carry the per-service status
	// the cross-server gate reads.
	spec, err := json.Marshal(map[string]any{
		"compose": map[string]any{"services": []map[string]any{
			{"name": "db", "image": image, "serverId": dataHost.serverID},
			{"name": "web", "image": image, "serverId": appHost.serverID, "dependsOn": []string{"db"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: appHost.serverID, Name: "shopapp", Kind: "app", Spec: spec,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer removeContainers("sigmahub-" + res.ID)

	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/shop",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: appHost.serverID,
		ConnectionID: conn.ID, Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []*fleetAgent{appHost, dataHost} {
		rec.ReconcileAsync(orgID, a.serverID)
	}

	// The gated service must not start before its dependency reports. Checked
	// against the real container list, not against the render: the whole failure
	// mode here is a difference between what the CP thinks and what is running.
	webContainer := "sigmahub-" + res.ID + "-web"
	dbContainer := "sigmahub-" + res.ID + "-db"
	waitFor(t, 60*time.Second, "the database service to come up on its own host",
		func() bool { return containerRunning(dbContainer) }, cpLog, dockerState("sigmahub-"+res.ID), dataHost.tail, appHost.tail)

	waitFor(t, 90*time.Second, "the dependent service to be released once its dependency reported",
		func() bool { return containerRunning(webContainer) }, cpLog, dockerState("sigmahub-"+res.ID), appHost.tail, dataHost.tail)

	// And the deployment completes. Before the fix it stayed 'queued' forever
	// with both containers running — the state that makes this worth testing
	// across the process boundary at all.
	waitFor(t, 60*time.Second, "the deployment to reach success", func() bool {
		got, gerr := st.GetDeployment(ctx, orgID, dep.ID)
		return gerr == nil && got.Status == "success"
	}, deploymentState(st, orgID, dep.ID), cpLog, dockerState("sigmahub-"+res.ID), appHost.tail, dataHost.tail)

	got, err := st.GetDeployment(ctx, orgID, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{"db", "web"} {
		if got.ServiceStatus[svc] != "success" {
			t.Fatalf("service %s status = %q, want success (%+v)", svc, got.ServiceStatus[svc], got.ServiceStatus)
		}
	}
}

// A cluster node reports what actually happened to it.
//
// The agent here cannot install k3s — there is no systemd in a test container —
// and that is the point: the failure has to reach the control plane and land on
// the node row with a message. Before this shipped, `clusters.status` was
// written once as 'provisioning' and nothing ever moved it, so a cluster whose
// control plane never came up was indistinguishable from one that did.
func TestFleetClusterNodeReportsItsFailure(t *testing.T) {
	fleetGate(t)
	st, dsdKey := testStore(t)
	ctx := context.Background()
	sigmad := buildSigmad(t)

	ts, rec, cpLog := fleetCP(t, st, dsdKey)
	orgID := "org_fleet_cluster"

	proj, err := st.CreateProject(ctx, orgID, "platform", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	node := startAgent(t, st, sigmad, ts.URL, orgID, "fleet-node")
	if err := st.AttachServer(ctx, orgID, env.ID, node.serverID, "test"); err != nil {
		t.Fatal(err)
	}
	// The k8s.node op is held until the node has a mesh address — without one
	// the API server would bind to an undefined interface — so give it the
	// address its own enrollment would have assigned.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE servers SET mesh_ip = '10.88.0.2' WHERE id = $1`, node.serverID); err != nil {
		t.Fatal(err)
	}

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: env.ID, Name: "prod", ControlPlaneID: node.serverID,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cluster.Status != "provisioning" {
		t.Fatalf("new cluster status = %q", cluster.Status)
	}
	rec.ReconcileAsync(orgID, node.serverID)

	// Whatever the outcome, the node has to say something. A silent node is the
	// bug this replaced.
	waitFor(t, 90*time.Second, "the node to report its Kubernetes state", func() bool {
		list, lerr := st.ListClusters(ctx, orgID, "")
		if lerr != nil || len(list) != 1 || len(list[0].Nodes) != 1 {
			return false
		}
		return list[0].Nodes[0].NodeStatus != store.NodeStatusPending
	}, cpLog, node.tail)

	list, err := st.ListClusters(ctx, orgID, "")
	if err != nil {
		t.Fatal(err)
	}
	n := list[0].Nodes[0]
	if n.ReportedAt == nil {
		t.Fatal("a node that reported must carry a timestamp")
	}
	// In this environment the install cannot succeed, so the report must be an
	// error that says why — and the cluster must NOT read ready.
	if n.NodeStatus == store.NodeStatusError {
		if n.NodeMessage == "" {
			t.Fatal("a failed node must explain itself; an empty message is the same as no report")
		}
		if list[0].Status == "ready" {
			t.Fatalf("cluster reads %q with a broken control plane", list[0].Status)
		}
		return
	}
	// If it did come up (a host that really can run k3s), the cluster must be
	// ready and carry the endpoint kubectl dials.
	if list[0].Status != "ready" || list[0].APIEndpoint == "" {
		t.Fatalf("node reported ready but the cluster did not follow: status=%q endpoint=%q",
			list[0].Status, list[0].APIEndpoint)
	}
}
