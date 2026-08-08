// Package k8s applies the Kubernetes (k3s) ops: bringing a host up as a
// cluster node, and reconciling one workload through the control-plane node.
//
// Like every other subsystem here it is a TYPED op handler, never a generic
// shell escape: the control plane cannot ask this package to run an arbitrary
// command, only to converge a node or a workload onto a described state.
package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Op kinds — MUST match the control plane's dsd.KindK8s* strings byte-for-byte
// (the two modules can't share Go types, so the wire names are duplicated).
const (
	KindK8sNode  = "k8s.node"
	KindK8sApply = "k8s.apply"
)

// Node roles.
const (
	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"
)

// NodeSpec brings this host up as a cluster member.
type NodeSpec struct {
	ClusterID   string `json:"clusterId"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	JoinToken   string `json:"joinToken"`
	ServerURL   string `json:"serverUrl,omitempty"`
	AdvertiseIP string `json:"advertiseIp"`
}

// ApplySpec is one workload reconciled through the API server.
type ApplySpec struct {
	ResourceID   string            `json:"resourceId"`
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace"`
	Image        string            `json:"image"`
	Replicas     int               `json:"replicas"`
	Ports        []int             `json:"ports,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	SecretRefs   []SecretRef       `json:"secretRefs,omitempty"`
	Hosts        []string          `json:"hosts,omitempty"`
	DeploymentID string            `json:"deploymentId,omitempty"`
}

// SecretRef is a secret the CP resolves at apply time; the DSD carries only the
// name, never the value.
type SecretRef struct {
	Name   string `json:"name"`
	EnvVar bool   `json:"envVar"`
}

// Secret is a resolved secret value.
type Secret struct {
	Name   string
	Value  string
	EnvVar bool
}

// SecretFetcher resolves a resource's secrets over the authenticated agent
// channel, exactly as the container driver does.
type SecretFetcher func(ctx context.Context, resourceID string) ([]Secret, error)

// Driver applies the k8s ops. runner and installer are swapped in tests.
type Driver struct {
	// runner executes a binary with args (never a shell).
	runner func(ctx context.Context, name string, args ...string) ([]byte, error)
	// installScript fetches and runs the k3s installer. Separated from runner so
	// tests can assert install intent without downloading anything.
	installScript func(ctx context.Context, env []string, args ...string) error
	writeFile     func(path string, data []byte, perm os.FileMode) error
	mkdirAll      func(path string, perm os.FileMode) error
	fetchSecrets  SecretFetcher
	euid          int
	// binDir is where k3s and kubectl live; overridable for tests.
	binDir string
	// manifestDir is k3s's auto-apply directory: anything dropped here is
	// reconciled by the server. Using it instead of `kubectl apply` means a
	// workload survives an API-server restart without us re-running anything.
	manifestDir string
}

// NewDriver builds a driver bound to the real host.
func NewDriver(fetchSecrets SecretFetcher) *Driver {
	return &Driver{
		runner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		installScript: installK3s,
		writeFile:     os.WriteFile,
		mkdirAll:      os.MkdirAll,
		fetchSecrets:  fetchSecrets,
		euid:          os.Geteuid(),
		binDir:        "/usr/local/bin",
		manifestDir:   "/var/lib/rancher/k3s/server/manifests",
	}
}

// Register wires the typed handlers into the apply registry.
func (d *Driver) Register(reg *apply.Registry) {
	reg.Register(KindK8sNode, d.applyNode)
	reg.Register(KindK8sApply, d.applyWorkload)
}

// applyNode converges this host into its cluster role. Idempotent: an already
// installed node with the same role and server URL is left alone, so a resync
// does not restart the API server under running workloads.
func (d *Driver) applyNode(ctx context.Context, op dsd.Op) error {
	var spec NodeSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode k8s.node spec: %w", err)
	}
	if spec.JoinToken == "" || spec.AdvertiseIP == "" {
		return fmt.Errorf("k8s.node requires a join token and an advertise address")
	}
	if spec.Role != RoleControlPlane && spec.ServerURL == "" {
		return fmt.Errorf("a worker node requires the control-plane server URL")
	}
	if d.euid != 0 {
		return fmt.Errorf("installing a cluster node requires root")
	}

	service := "k3s"
	if spec.Role != RoleControlPlane {
		service = "k3s-agent"
	}
	// Already converged? The unit exists and is active, so nothing to do. This
	// is the common case on every resync and must not disturb the cluster.
	if d.serviceActive(ctx, service) {
		return nil
	}

	env := []string{
		"INSTALL_K3S_SKIP_START=false",
		"K3S_TOKEN=" + spec.JoinToken,
	}
	var args []string
	if spec.Role == RoleControlPlane {
		// Bind the API server to the mesh address only: the cluster is reachable
		// org-mesh-wide and nowhere else, the same invariant databases and object
		// storage already hold to. Traefik is disabled because SigmaHub already
		// runs its own proxy and two ingress controllers fighting over :80/:443
		// is not a state worth debugging.
		args = []string{
			"server",
			"--node-ip", spec.AdvertiseIP,
			"--advertise-address", spec.AdvertiseIP,
			"--bind-address", spec.AdvertiseIP,
			"--disable", "traefik",
			"--write-kubeconfig-mode", "0600",
		}
	} else {
		env = append(env, "K3S_URL="+spec.ServerURL)
		args = []string{"agent", "--node-ip", spec.AdvertiseIP}
	}
	if err := d.installScript(ctx, env, args...); err != nil {
		return fmt.Errorf("install k3s %s: %w", spec.Role, err)
	}
	return nil
}

// serviceActive reports whether a systemd unit is running.
func (d *Driver) serviceActive(ctx context.Context, unit string) bool {
	out, err := d.runner(ctx, "systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// applyWorkload writes the workload's manifests into k3s's auto-apply
// directory. The server reconciles them, so the workload survives an API-server
// restart with no action from us — and removing the file removes the workload.
func (d *Driver) applyWorkload(ctx context.Context, op dsd.Op) error {
	var spec ApplySpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode k8s.apply spec: %w", err)
	}
	if spec.Name == "" || spec.Image == "" {
		return fmt.Errorf("k8s.apply requires a name and an image")
	}
	if !dnsName.MatchString(spec.Name) {
		return fmt.Errorf("workload name %q is not a valid Kubernetes name", spec.Name)
	}
	ns := spec.Namespace
	if ns == "" {
		ns = "default"
	}
	if !dnsName.MatchString(ns) {
		return fmt.Errorf("namespace %q is not a valid Kubernetes name", ns)
	}

	// Resolve secrets at apply time so the DSD never carried a value.
	var secrets []Secret
	if len(spec.SecretRefs) > 0 && d.fetchSecrets != nil {
		fetched, err := d.fetchSecrets(ctx, spec.ResourceID)
		if err != nil {
			return fmt.Errorf("resolve secrets: %w", err)
		}
		want := map[string]bool{}
		for _, ref := range spec.SecretRefs {
			want[ref.Name] = true
		}
		for _, sec := range fetched {
			if want[sec.Name] {
				secrets = append(secrets, sec)
			}
		}
	}

	manifest, err := renderManifests(spec, ns, secrets)
	if err != nil {
		return err
	}
	if err := d.ensureDir(d.manifestDir, 0o700); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	path := filepath.Join(d.manifestDir, "sigmahub-"+spec.ResourceID+".yaml")
	// 0600: the manifest embeds resolved secret values.
	if err := d.writeFile(path, []byte(manifest), 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

func (d *Driver) ensureDir(path string, perm os.FileMode) error {
	if d.mkdirAll != nil {
		return d.mkdirAll(path, perm)
	}
	return os.MkdirAll(path, perm)
}

// dnsName is the RFC 1123 label rule Kubernetes enforces on object names.
// Validating here turns a bad name into a clear op failure instead of a
// manifest the API server silently rejects.
var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// installK3s runs the official installer with the given env and server args.
// The installer URL is fixed — this is not a generic download-and-run.
func installK3s(ctx context.Context, env []string, args ...string) error {
	script, err := fetchInstaller(ctx)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "k3s-install-*.sh")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(script); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{tmp.Name()}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
