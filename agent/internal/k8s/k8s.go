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
	"time"

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
	ResourceID string `json:"resourceId"`
	// Service is the Compose service this workload came from, empty for a
	// single-container app.
	Service      string            `json:"service,omitempty"`
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace"`
	Image        string            `json:"image"`
	Replicas     int               `json:"replicas"`
	Ports        []int             `json:"ports,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	SecretRefs   []SecretRef       `json:"secretRefs,omitempty"`
	Hosts        []string          `json:"hosts,omitempty"`
	DeploymentID string            `json:"deploymentId,omitempty"`
	// RegistryHost, when set, means the image lives in a private registry: the
	// agent fetches the credential over its own authenticated channel and
	// renders it as an imagePullSecret. The DSD never carries the password.
	RegistryHost string `json:"registryHost,omitempty"`
	// Workloads is every workload name this resource should currently have.
	// Manifests for this resource that are not in the list are removed, so a
	// service deleted from a Compose file stops running instead of outliving
	// its own definition.
	Workloads []string `json:"workloads,omitempty"`
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

// RegistryCredential authenticates an image pull from a private registry.
type RegistryCredential struct {
	Host     string
	Username string
	Password string
}

// RegistryFetcher resolves the org's registry credential over the authenticated
// agent channel. Nil on a host that never pulls a private image.
type RegistryFetcher func(ctx context.Context) (RegistryCredential, error)

// NodeReport is what this node tells the control plane about k3s on it.
type NodeReport struct {
	ClusterID   string
	Ready       bool
	Message     string
	APIEndpoint string
	Version     string
}

// NodeReporter delivers a NodeReport to the control plane. Without one a
// cluster has no way to leave 'provisioning': the node is the only thing that
// knows whether k3s actually came up on it.
type NodeReporter func(ctx context.Context, rep NodeReport) error

// Driver applies the k8s ops. runner and installer are swapped in tests.
type Driver struct {
	// runner executes a binary with args (never a shell).
	runner func(ctx context.Context, name string, args ...string) ([]byte, error)
	// installScript fetches and runs the k3s installer. Separated from runner so
	// tests can assert install intent without downloading anything.
	installScript func(ctx context.Context, env []string, args ...string) error
	writeFile     func(path string, data []byte, perm os.FileMode) error
	mkdirAll      func(path string, perm os.FileMode) error
	removeFile    func(path string) error
	readDir       func(path string) ([]string, error)
	fetchSecrets  SecretFetcher
	fetchRegistry RegistryFetcher
	report        NodeReporter
	euid          int
	// binDir is where k3s and kubectl live; overridable for tests.
	binDir string
	// manifestDir is k3s's auto-apply directory: anything dropped here is
	// reconciled by the server. Using it instead of `kubectl apply` means a
	// workload survives an API-server restart without us re-running anything.
	manifestDir string
	// rolloutTimeout / rolloutInterval bound the wait for a written manifest to
	// become a running Deployment. Overridable so tests do not sleep.
	rolloutTimeout  time.Duration
	rolloutInterval time.Duration
}

// NewDriver builds a driver bound to the real host.
func NewDriver(fetchSecrets SecretFetcher, fetchRegistry RegistryFetcher, report NodeReporter) *Driver {
	return &Driver{
		runner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		installScript: installK3s,
		writeFile:     os.WriteFile,
		mkdirAll:      os.MkdirAll,
		removeFile:    os.Remove,
		readDir: func(path string) ([]string, error) {
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				if !e.IsDir() {
					names = append(names, e.Name())
				}
			}
			return names, nil
		},
		fetchSecrets:  fetchSecrets,
		fetchRegistry: fetchRegistry,
		report:        report,
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
//
// Every path through here reports back. Before that, a cluster was written as
// 'provisioning' at creation and nothing ever moved it: a cluster that came up
// perfectly and one whose install failed on the first line looked identical in
// the dashboard, and the only way to tell them apart was to SSH in. The report
// is the node's own account — installed, service running, and on the control
// plane the API server actually answering — not an inference from "the op did
// not return an error".
func (d *Driver) applyNode(ctx context.Context, op dsd.Op) (err error) {
	var spec NodeSpec
	if jerr := json.Unmarshal(op.Spec, &spec); jerr != nil {
		return fmt.Errorf("decode k8s.node spec: %w", jerr)
	}
	// Report on the way out, whatever happened. A failure the control plane
	// never hears about is the exact state this is here to end.
	defer func() {
		if spec.ClusterID == "" {
			return
		}
		if err != nil {
			d.reportNode(ctx, NodeReport{ClusterID: spec.ClusterID, Ready: false, Message: err.Error()})
			return
		}
		d.reportNode(ctx, d.probeNode(ctx, spec))
	}()

	if spec.JoinToken == "" || spec.AdvertiseIP == "" {
		return fmt.Errorf("k8s.node requires a join token and an advertise address")
	}
	if spec.Role != RoleControlPlane && spec.ServerURL == "" {
		return fmt.Errorf("a worker node requires the control-plane server URL")
	}
	if d.euid != 0 {
		return fmt.Errorf("installing a cluster node requires root")
	}

	// Already converged? The unit exists and is active, so nothing to do. This
	// is the common case on every resync and must not disturb the cluster.
	if d.serviceActive(ctx, nodeService(spec.Role)) {
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

// nodeService is the systemd unit for a role.
func nodeService(role string) string {
	if role == RoleControlPlane {
		return "k3s"
	}
	return "k3s-agent"
}

// probeNode asks the host what state k3s is actually in.
//
// A worker can only say whether its unit is running — it has no API server to
// interrogate, and dialling the control plane's would prove something about
// that node, not this one. The control plane goes further: an active unit is
// not the same as a serving API server (k3s is "active" while it is still
// starting), so it asks kubectl for the version and reports ready only when it
// gets an answer.
func (d *Driver) probeNode(ctx context.Context, spec NodeSpec) NodeReport {
	rep := NodeReport{ClusterID: spec.ClusterID}
	if !d.serviceActive(ctx, nodeService(spec.Role)) {
		rep.Message = "k3s is installed but its service is not running"
		return rep
	}
	if spec.Role != RoleControlPlane {
		rep.Ready = true
		return rep
	}
	rep.APIEndpoint = "https://" + spec.AdvertiseIP + ":6443"
	out, err := d.runner(ctx, filepath.Join(d.binDir, "kubectl"), "version", "-o", "json")
	if err != nil {
		rep.Message = "the API server is not answering yet: " + firstLine(string(out))
		return rep
	}
	var ver struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if jerr := json.Unmarshal(out, &ver); jerr != nil || ver.ServerVersion.GitVersion == "" {
		// kubectl answered but said nothing about a server: it reached the
		// client only, so there is no cluster to schedule onto.
		rep.Message = "the API server did not report a version"
		return rep
	}
	rep.Ready = true
	rep.Version = ver.ServerVersion.GitVersion
	return rep
}

// reportNode delivers a report, logging nothing on failure: the reporter itself
// owns retry, and a node whose report is lost is re-reported on the next resync
// because applyNode runs again.
func (d *Driver) reportNode(ctx context.Context, rep NodeReport) {
	if d.report == nil || rep.ClusterID == "" {
		return
	}
	_ = d.report(ctx, rep)
}

// firstLine trims command output down to something a status field can hold.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// serviceActive reports whether a systemd unit is running.
func (d *Driver) serviceActive(ctx context.Context, unit string) bool {
	out, err := d.runner(ctx, "systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// applyWorkload writes the workload's manifests into k3s's auto-apply
// directory, then waits for the Deployment to actually become available. The
// server reconciles the directory, so the workload survives an API-server
// restart with no action from us — and removing the file removes the workload.
//
// The wait is the point. Writing a file is not running a workload: an image the
// nodes cannot pull, a container that crash-loops, a pod nothing can schedule
// and a namespace over quota all leave the manifest sitting there perfectly
// applied. Returning nil at the write reported those as APPLIED, which turned
// the op into a 'success' deployment (rollout phase → AdvanceDeploymentForResource),
// a green resource badge and a rollback target — while every pod was in
// ImagePullBackOff and the custom domain 502'd. The container path has always
// held itself to this bar (gateHealthy + captureStartupLogs); the cluster path
// now does too.
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

	// A private registry needs credentials on the NODE that runs the pod, not
	// just on whoever built the image, so they ride into the manifest as an
	// imagePullSecret. Fetched here, at apply time, so the DSD never held them.
	var pull *RegistryCredential
	if spec.RegistryHost != "" && d.fetchRegistry != nil {
		cred, err := d.fetchRegistry(ctx)
		if err != nil {
			return fmt.Errorf("resolve registry credential: %w", err)
		}
		if cred.Username != "" || cred.Password != "" {
			if cred.Host == "" {
				cred.Host = spec.RegistryHost
			}
			pull = &cred
		}
	}

	manifest, err := renderManifests(spec, ns, secrets, pull)
	if err != nil {
		return err
	}
	if err := d.ensureDir(d.manifestDir, 0o700); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	// 0600: the manifest embeds resolved secret values.
	if err := d.writeFile(filepath.Join(d.manifestDir, manifestFile(spec.Name)), []byte(manifest), 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	// Reconcile the resource's manifests down to the workloads it should have.
	// A Compose service removed from the file has to stop running; k3s keeps
	// applying whatever is in this directory, so leaving the file behind leaves
	// the Deployment alive with nothing in the product describing it.
	d.pruneManifests(spec)
	// The manifest stays on disk whatever the gate says: k3s keeps retrying it,
	// so a rollout that completes after our deadline still converges, and the
	// next resync re-runs this op and reports the better answer.
	return d.gateRollout(ctx, ns, spec.Name)
}

// rollout gate defaults. Five minutes is the same order as a first pull of a
// large image onto a cold node; the interval is short enough that a fast deploy
// is not held open waiting for a poll.
const (
	defaultRolloutTimeout  = 5 * time.Minute
	defaultRolloutInterval = 3 * time.Second
)

// gateRollout blocks until the Deployment reports itself rolled out, or fails
// with the cluster's own account of why it did not.
//
// `kubectl rollout status` is asked with a per-attempt timeout rather than one
// long watch, because the manifest has only just been written: for the first
// second or two the Deployment does not exist yet and kubectl exits NotFound.
// Retrying until the outer deadline covers that window and a genuinely stuck
// rollout with the same loop.
func (d *Driver) gateRollout(ctx context.Context, ns, name string) error {
	if d.runner == nil {
		return nil
	}
	timeout := d.rolloutTimeout
	if timeout <= 0 {
		timeout = defaultRolloutTimeout
	}
	interval := d.rolloutInterval
	if interval <= 0 {
		interval = defaultRolloutInterval
	}
	kubectl := filepath.Join(d.binDir, "kubectl")
	deadline := time.Now().Add(timeout)
	var last string
	for {
		out, err := d.runner(ctx, kubectl, "-n", ns, "rollout", "status",
			"deployment/"+name, "--timeout="+interval.String())
		if err == nil {
			return nil
		}
		if l := firstLine(string(out)); l != "" {
			last = l
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			// The operator has no other window into the cluster, so the failure
			// carries what a human would have run by hand: the Deployment's and
			// the pods' events (ImagePullBackOff, unschedulable, quota) and the
			// pods' own last words (a crash loop's panic).
			return fmt.Errorf("workload %s/%s did not become available within %s: %s%s",
				ns, name, timeout, last, d.rolloutDiagnostics(ctx, kubectl, ns, name))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// rolloutDiagnosticChars bounds each collected block. The tail is what matters —
// events are appended, and a panic is at the end of the log.
const rolloutDiagnosticChars = 2000

// rolloutDiagnostics collects why the rollout is stuck. Every error here is
// swallowed on purpose: this runs on the way to reporting a rollout failure and
// must never replace that error with a worse one about collecting diagnostics.
func (d *Driver) rolloutDiagnostics(ctx context.Context, kubectl, ns, name string) string {
	var b strings.Builder
	collect := func(label string, args ...string) {
		out, err := d.runner(ctx, kubectl, args...)
		text := strings.TrimSpace(string(out))
		if text == "" {
			if err == nil {
				return
			}
			text = err.Error()
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s", label, tail(text, rolloutDiagnosticChars))
	}
	collect("describe deployment", "-n", ns, "describe", "deployment/"+name)
	collect("describe pods", "-n", ns, "describe", "pods", "-l", "app="+name)
	collect("pod logs", "-n", ns, "logs", "-l", "app="+name,
		"--tail="+fmt.Sprint(rolloutLogLines), "--all-containers=true")
	return b.String()
}

// rolloutLogLines caps what a crash-looping pod can push into the deploy log,
// mirroring the container path's startup-log tail.
const rolloutLogLines = 100

// tail keeps the last n characters, clipped to a line boundary so the output
// does not start mid-word.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "…\n" + s
}

// manifestFile is the manifest a workload owns. Keyed by the workload's own
// name, which is unique per Compose service — one file per service, so two
// services of the same app do not overwrite each other.
func manifestFile(name string) string { return name + ".yaml" }

// pruneManifests removes manifests belonging to this resource that are not in
// the spec's Workloads set. Every op for a resource carries the same set, so
// the result is the same whichever one runs last, and an op with no set (an
// older control plane) prunes nothing.
func (d *Driver) pruneManifests(spec ApplySpec) {
	if len(spec.Workloads) == 0 || d.readDir == nil || d.removeFile == nil {
		return
	}
	keep := make(map[string]bool, len(spec.Workloads))
	for _, w := range spec.Workloads {
		keep[manifestFile(w)] = true
	}
	// A resource's workload names all start with its own name, and identifiers
	// are fixed-length, so no other resource's manifests can match this prefix.
	prefix := manifestFile(spec.Name)
	prefix = strings.TrimSuffix(prefix, ".yaml")
	if spec.Service != "" {
		// This op is one service, so the shared prefix is the resource part —
		// everything up to the service suffix.
		prefix = strings.TrimSuffix(prefix, "-"+sanitizedService(spec))
	}
	names, err := d.readDir(d.manifestDir)
	if err != nil {
		return // nothing to prune against; the next resync tries again
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".yaml") || !strings.HasPrefix(name, prefix) {
			continue
		}
		if keep[name] {
			continue
		}
		_ = d.removeFile(filepath.Join(d.manifestDir, name))
	}
}

// sanitizedService is the service suffix as it appears in the workload name.
// The control plane builds the name from the same service string, rewriting
// anything not [a-z0-9-]; mirroring that here is what lets the resource prefix
// be recovered from the workload name.
func sanitizedService(spec ApplySpec) string {
	var b strings.Builder
	for _, r := range strings.ToLower(spec.Service) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return b.String()
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
