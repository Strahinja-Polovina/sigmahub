package container

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// The real-Docker e2e rig. It drives the container driver against a live Docker
// daemon and asserts the exact `docker inspect` values the isolation defaults
// promise. It runs only when SIGMAD_DOCKER_E2E=1 AND a daemon is reachable (CI
// sets both on the linux/amd64 leg), so `go test ./...` stays green on hosts
// without Docker.

const (
	e2eImage     = "nginxinc/nginx-unprivileged:1.27-alpine"
	e2eNetwork   = "sigmahub-e2e-net"
	e2eResource  = "res_e2e"
	e2eContainer = "sigmahub-res_e2e"
	e2eVolume    = "sigmahub-res_e2e-data"
)

func e2eDriver(t *testing.T) (*Driver, *DockerClient) {
	t.Helper()
	if os.Getenv("SIGMAD_DOCKER_E2E") == "" {
		t.Skip("set SIGMAD_DOCKER_E2E=1 to run the real-Docker e2e")
	}
	docker := NewDockerClient("", os.Getenv("DOCKER_HOST"))
	if avail, _ := Probe(context.Background(), docker); !avail {
		t.Skip("docker daemon not reachable")
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d := NewDriver(docker, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return d, docker
}

func e2eCleanup(ctx context.Context, docker *DockerClient) {
	if c, ok, _ := docker.ContainerInspect(ctx, e2eContainer); ok {
		_ = docker.ContainerRemove(ctx, c.ID, true)
	}
	_ = docker.VolumeRemove(ctx, e2eVolume, true)
}

func opFor(t *testing.T, kind, id string, deps []string, spec any) dsd.Op {
	t.Helper()
	b, _ := json.Marshal(spec)
	return dsd.Op{ID: id, Kind: kind, DependsOn: deps, Spec: b}
}

func e2eContainerSpec(readOnly, privileged bool) ContainerSpec {
	s := ContainerSpec{
		ResourceID:     e2eResource,
		Name:           e2eContainer,
		Image:          e2eImage,
		Network:        e2eNetwork,
		User:           "101:101",
		ReadOnlyRootfs: readOnly,
		Tmpfs:          []string{"/tmp", "/var/cache/nginx", "/var/run"},
		CPUs:           0.5,
		MemoryMB:       256,
		Ports:          []PortMapping{{Container: 8080}},
	}
	s.Privileged = privileged
	return s
}

// rawInspect reaches the daemon directly (same-package access to the unexported
// client) so the test can assert HostConfig isolation fields.
func rawInspect(t *testing.T, docker *DockerClient, name string) struct {
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		User   string            `json:"User"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		SecurityOpt    []string `json:"SecurityOpt"`
		ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
		NanoCpus       int64    `json:"NanoCpus"`
		Memory         int64    `json:"Memory"`
		NetworkMode    string   `json:"NetworkMode"`
		Privileged     bool     `json:"Privileged"`
	} `json:"HostConfig"`
} {
	t.Helper()
	var out struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			User   string            `json:"User"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			SecurityOpt    []string `json:"SecurityOpt"`
			ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
			NanoCpus       int64    `json:"NanoCpus"`
			Memory         int64    `json:"Memory"`
			NetworkMode    string   `json:"NetworkMode"`
			Privileged     bool     `json:"Privileged"`
		} `json:"HostConfig"`
	}
	if err := docker.do(context.Background(), http.MethodGet, "/containers/"+name+"/json", nil, &out); err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	return out
}

func waitRunning(t *testing.T, docker *DockerClient, name string) {
	t.Helper()
	for i := 0; i < 30; i++ {
		if c, ok, _ := docker.ContainerInspect(context.Background(), name); ok && c.Running {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("container %s did not reach running", name)
}

func hasOpt(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}

func TestDockerE2EConvergeIsolationDriftGC(t *testing.T) {
	d, docker := e2eDriver(t)
	ctx := context.Background()
	e2eCleanup(ctx, docker)
	t.Cleanup(func() { e2eCleanup(context.Background(), docker) })

	// Network, image, container ops (as the CP would render them).
	if err := d.opNetworkEnsure(ctx, opFor(t, KindNetworkEnsure, "net", nil, NetworkSpec{Name: e2eNetwork})); err != nil {
		t.Fatalf("network ensure: %v", err)
	}
	if err := d.opImagePull(ctx, opFor(t, KindImagePull, "img", nil, ImageSpec{Image: e2eImage})); err != nil {
		t.Fatalf("image pull: %v", err)
	}
	if err := d.opContainerApply(ctx, opFor(t, KindContainerApply, "res:"+e2eResource, []string{"net", "img"}, e2eContainerSpec(true, false))); err != nil {
		t.Fatalf("container apply: %v", err)
	}
	waitRunning(t, docker, e2eContainer)

	// Assert the exact isolation values.
	insp := rawInspect(t, docker, e2eContainer)
	if !insp.State.Running {
		t.Fatal("container not running")
	}
	if !hasOpt(insp.HostConfig.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("missing no-new-privileges: %v", insp.HostConfig.SecurityOpt)
	}
	if !hasOpt(insp.HostConfig.SecurityOpt, "apparmor=docker-default") {
		t.Errorf("missing apparmor=docker-default: %v", insp.HostConfig.SecurityOpt)
	}
	if hasOpt(insp.HostConfig.SecurityOpt, "seccomp=unconfined") {
		t.Error("seccomp must not be unconfined (default profile expected)")
	}
	if !insp.HostConfig.ReadonlyRootfs {
		t.Error("expected read-only rootfs")
	}
	if insp.HostConfig.Privileged {
		t.Error("container must not be privileged")
	}
	if insp.HostConfig.NanoCpus != 500000000 {
		t.Errorf("NanoCpus = %d, want 5e8", insp.HostConfig.NanoCpus)
	}
	if insp.HostConfig.Memory != 256*1024*1024 {
		t.Errorf("Memory = %d, want 256Mi", insp.HostConfig.Memory)
	}
	if insp.Config.User != "101:101" {
		t.Errorf("User = %q, want non-root 101:101", insp.Config.User)
	}
	if insp.HostConfig.NetworkMode != e2eNetwork {
		t.Errorf("NetworkMode = %q, want %q", insp.HostConfig.NetworkMode, e2eNetwork)
	}
	if insp.Config.Labels[LabelManaged] != "true" {
		t.Error("missing managed label")
	}

	// Idempotent: re-apply the same spec → container is left running (same id).
	before, _, _ := docker.ContainerInspect(ctx, e2eContainer)
	if err := d.opContainerApply(ctx, opFor(t, KindContainerApply, "res:"+e2eResource, []string{"net", "img"}, e2eContainerSpec(true, false))); err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	after, _, _ := docker.ContainerInspect(ctx, e2eContainer)
	if before.ID != after.ID {
		t.Error("idempotent apply recreated the container")
	}

	// Drift repair: stop the container out of band, reconcile → running again.
	if err := docker.ContainerStop(ctx, after.ID, time.Second); err != nil {
		t.Fatalf("drift stop: %v", err)
	}
	d.Reconcile(ctx)
	waitRunning(t, docker, e2eContainer)

	// GC: an empty document removes the managed container.
	d.GC(ctx, dsd.Document{Version: 99, Ops: nil})
	if _, ok, _ := docker.ContainerInspect(ctx, e2eContainer); ok {
		t.Error("GC did not remove the orphaned container")
	}
}

func TestDockerE2EPolicyDenial(t *testing.T) {
	d, docker := e2eDriver(t)
	ctx := context.Background()
	e2eCleanup(ctx, docker)
	t.Cleanup(func() { e2eCleanup(context.Background(), docker) })

	// A privileged container is refused locally with a typed policy failure —
	// even though the op reached the agent — and no container is created.
	err := d.opContainerApply(ctx, opFor(t, KindContainerApply, "res:"+e2eResource, nil, e2eContainerSpec(false, true)))
	if err == nil {
		t.Fatal("privileged container was not refused")
	}
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("want *PolicyError, got %T: %v", err, err)
	}
	if _, ok, _ := docker.ContainerInspect(ctx, e2eContainer); ok {
		t.Error("a policy-denied container must not exist")
	}
}

func TestDockerE2EVolumeLifecycle(t *testing.T) {
	d, docker := e2eDriver(t)
	ctx := context.Background()
	_ = docker.VolumeRemove(ctx, e2eVolume, true)
	t.Cleanup(func() { _ = docker.VolumeRemove(context.Background(), e2eVolume, true) })

	if err := d.opVolumeEnsure(ctx, opFor(t, KindVolumeEnsure, "vol", nil, VolumeSpec{Name: e2eVolume, ResourceID: e2eResource})); err != nil {
		t.Fatalf("volume ensure: %v", err)
	}
	if ok, _ := docker.VolumeExists(ctx, e2eVolume); !ok {
		t.Fatal("volume not created")
	}
	if err := d.opVolumeRemove(ctx, opFor(t, KindVolumeRemove, "volrm:pdo", nil, VolumeSpec{Name: e2eVolume})); err != nil {
		t.Fatalf("volume remove: %v", err)
	}
	if ok, _ := docker.VolumeExists(ctx, e2eVolume); ok {
		t.Fatal("volume not removed")
	}
}
