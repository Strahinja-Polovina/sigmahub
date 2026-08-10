package k8s

// The real-Kubernetes e2e.
//
// The manifests here are hand-rolled YAML, on purpose — the agent ships as one
// static binary and pulling in the Kubernetes client libraries to emit five
// fixed object kinds is not a trade worth making. The cost of that choice is
// that nothing in the type system says the output is valid: one wrong
// indentation level, one field in the wrong place, and every cluster deploy
// fails at the API server with the product reporting a successful apply.
//
// So this feeds what we actually render to a real API server and asserts it is
// accepted. It needs no images and schedules no pods — `k3s server
// --disable-agent` is a full control plane on its own — which is what makes it
// runnable in CI rather than only on a real cluster.
//
// Runs when SIGMAD_K8S_E2E=1 and KUBECONFIG points at a reachable cluster;
// SIGMAD_KUBECTL overrides the kubectl binary (k3s ships one as `k3s kubectl`).

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// e2eNamespace is unique per run. Deleting a namespace is asynchronous — it
// sits in Terminating and refuses new content for a while — so reusing a fixed
// name makes the SECOND run of the suite fail on the first one's cleanup. In CI
// that reads as a real failure and a re-run does not clear it.
func e2eNamespace(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func e2eKubectl(t *testing.T) func(stdin string, args ...string) (string, error) {
	t.Helper()
	if os.Getenv("SIGMAD_K8S_E2E") == "" {
		t.Skip("set SIGMAD_K8S_E2E=1 (and KUBECONFIG) to run the real-Kubernetes e2e")
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("KUBECONFIG is not set")
	}
	bin := os.Getenv("SIGMAD_KUBECTL")
	var base []string
	if bin == "" {
		bin = "kubectl"
	} else if strings.HasSuffix(bin, "k3s") {
		base = []string{"kubectl"} // the k3s binary fronts kubectl as a subcommand
	}
	return func(stdin string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append(append([]string{}, base...), args...)...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
}

// The single-container workload: namespace, secrets (env and file mode), a
// private-registry pull secret, ports, and an ingress. Every object we know how
// to emit, in one document.
//
// The spec is the CONTROL PLANE's (SIGMA-338): it is decoded from the fixture
// cp/internal/reconciler writes, not written down here. A literal proves only
// that the renderer handles what the test author typed; what the API server
// has to accept is what the control plane actually sends, and the two structs
// are hand-mirrored across a module boundary with no import to keep them
// honest. Anything the control plane stops sending — a lost replica count, a
// renamed hosts field that silently drops every ingress — now fails here.
func TestK8sE2EManifestAcceptedByAPIServer(t *testing.T) {
	kubectl := e2eKubectl(t)

	spec := cpApplySpec(t, "git-deployed-app", "")
	// Two overrides, both about the test environment rather than the workload:
	// a namespace unique to this run (deleting one is asynchronous, so a fixed
	// name makes the SECOND run fail on the first one's cleanup), and env values
	// containing a colon and a newline. The control plane would never send those
	// — but the renderer's quoting is what stops a secret from rewriting the
	// document, and this is the only place a real parser checks it.
	spec.Namespace = e2eNamespace("sigmahub-prj")
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env["WITH_COLON"] = "a:b"
	spec.Env["WITH_NEWLINE"] = "a\nb"
	// A workload the control plane asked for must be able to have a pod. The
	// API server accepts replicas: 0 without complaint and runs nothing, which
	// is exactly the class of failure a green-everywhere suite misses.
	if spec.Replicas < 1 {
		t.Fatalf("the control plane rendered replicas = %d", spec.Replicas)
	}
	if len(spec.Hosts) == 0 || len(spec.Ports) == 0 {
		t.Fatalf("fixture case lost its hosts/ports; nothing would exercise the ingress: %+v", spec)
	}
	secrets := []Secret{
		{Name: "DATABASE_URL", Value: "postgres://u:p@h:5432/db\nevil: true", EnvVar: true},
		{Name: "TLS_KEY", Value: "-----BEGIN KEY-----\nabc\n-----END KEY-----"},
	}
	pull := &RegistryCredential{Host: "ghcr.io", Username: "bot", Password: "s3cret"}

	manifest, err := renderManifests(spec, spec.Namespace, secrets, pull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = kubectl("", "delete", "namespace", spec.Namespace, "--ignore-not-found", "--wait=false")
	}()

	if out, err := kubectl(manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("the API server rejected our manifest: %v\n%s\n--- manifest ---\n%s", err, out, manifest)
	}

	// Applied is not the same as correct: read the objects back and check the
	// values actually landed where the deploy depends on them being.
	out, err := kubectl("", "get", "deployment", spec.Name, "-n", spec.Namespace,
		"-o", "jsonpath={.spec.replicas} {.spec.template.spec.containers[0].image} {.spec.template.spec.imagePullSecrets[0].name}")
	if err != nil {
		t.Fatalf("deployment not readable: %v\n%s", err, out)
	}
	want := fmt.Sprintf("%d %s %s-registry", spec.Replicas, spec.Image, spec.Name)
	if strings.TrimSpace(out) != want {
		t.Fatalf("deployment = %q, want %q", strings.TrimSpace(out), want)
	}

	// A secret value containing YAML structure must have survived as DATA, not
	// been parsed as part of the document.
	out, err = kubectl("", "get", "secret", spec.Name+"-secrets", "-n", spec.Namespace,
		"-o", "jsonpath={.data.DATABASE_URL}")
	if err != nil {
		t.Fatalf("secret not readable: %v\n%s", err, out)
	}
	decoded, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if derr != nil || string(decoded) != secrets[0].Value {
		t.Fatalf("secret round-trip = %q (err %v), want %q", decoded, derr, secrets[0].Value)
	}

	// The pull secret has to be a real dockerconfigjson, or the kubelet ignores
	// it and every private image is an ImagePullBackOff.
	out, err = kubectl("", "get", "secret", spec.Name+"-registry", "-n", spec.Namespace, "-o", "jsonpath={.type}")
	if err != nil || strings.TrimSpace(out) != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("pull secret type = %q (err %v)", strings.TrimSpace(out), err)
	}

	// Env vars with a colon and a newline must not have become structure.
	out, err = kubectl("", "get", "deployment", spec.Name, "-n", spec.Namespace,
		"-o", `jsonpath={.spec.template.spec.containers[0].env[?(@.name=="WITH_NEWLINE")].value}`)
	if err != nil || out != "a\nb" {
		t.Fatalf("env value round-trip = %q (err %v), want %q", out, err, "a\nb")
	}

	// The domain the control plane attached must be the domain the API server
	// ended up routing — not a hostname this test invented.
	if out, err := kubectl("", "get", "ingress", spec.Name, "-n", spec.Namespace,
		"-o", "jsonpath={.spec.rules[0].host}"); err != nil || strings.TrimSpace(out) != spec.Hosts[0] {
		t.Fatalf("ingress host = %q (err %v), want %q", strings.TrimSpace(out), err, spec.Hosts[0])
	}
}

// A Compose app is N workloads in one namespace. They must not collide: same
// namespace, distinct object names, each its own Deployment and Service.
//
// The workloads are the ones the control plane renders for a real Compose
// graph (SIGMA-338) — read from its fixture rather than invented here, so the
// object names the API server sees are the names dsd.K8sWorkloadName produces
// and the portless service is portless because the control plane said so.
func TestK8sE2EComposeWorkloadsCoexist(t *testing.T) {
	kubectl := e2eKubectl(t)
	ns := e2eNamespace("sigmahub-prj-compose")
	defer func() { _, _ = kubectl("", "delete", "namespace", ns, "--ignore-not-found", "--wait=false") }()

	specs := cpApplySpecs(t, "compose-graph")
	if len(specs) < 2 {
		t.Fatalf("the compose fixture has %d workloads; it is meant to be a graph", len(specs))
	}
	var portless []string
	for _, spec := range specs {
		spec.Namespace = ns
		if len(spec.Ports) == 0 {
			portless = append(portless, spec.Name)
		}
		manifest, err := renderManifests(spec, ns, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := kubectl(manifest, "apply", "-f", "-"); err != nil {
			t.Fatalf("workload %s rejected: %v\n%s\n--- manifest ---\n%s", spec.Name, err, out, manifest)
		}
	}
	if len(portless) == 0 {
		t.Fatal("the compose fixture has no portless workload; the no-Service rule below is untested")
	}

	out, err := kubectl("", "get", "deployments", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		if !strings.Contains(out, spec.Name) {
			t.Fatalf("deployments = %q, missing %s", out, spec.Name)
		}
	}
	// A portless worker has nothing to expose; emitting a Service with no ports
	// is rejected by the API server, which is why the renderer omits it.
	svcOut, err := kubectl("", "get", "services", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range portless {
		if strings.Contains(svcOut, name) {
			t.Fatalf("a portless workload must not get a Service object: %q", svcOut)
		}
	}
}

// probeNode parses `kubectl version -o json`. Against the real binary, because
// the whole point of the probe is that an active systemd unit is not the same
// as a serving API server — and a parse that silently fails would report every
// healthy control plane as broken.
func TestK8sE2ENodeProbeReadsTheRealAPIServer(t *testing.T) {
	kubectl := e2eKubectl(t)
	// Prove the cluster is actually up before asserting the probe agrees.
	if out, err := kubectl("", "version", "-o", "json"); err != nil {
		t.Fatalf("cluster not reachable: %v\n%s", err, out)
	}

	bin := os.Getenv("SIGMAD_KUBECTL")
	if bin == "" {
		bin = "kubectl"
	}
	d, _ := testDriver(t)
	d.binDir = ""
	d.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if !strings.HasSuffix(name, "kubectl") {
			return []byte("active\n"), nil // the systemd probe
		}
		if strings.HasSuffix(bin, "k3s") {
			args = append([]string{"kubectl"}, args...)
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
		return cmd.CombinedOutput()
	}
	// binDir "" makes filepath.Join produce a bare "kubectl", which the stub
	// above routes to the real binary.

	rep := d.probeNode(context.Background(), NodeSpec{
		ClusterID: "c1", Role: RoleControlPlane, AdvertiseIP: "127.0.0.1",
	})
	if !rep.Ready {
		t.Fatalf("a live API server must report ready: %+v", rep)
	}
	if !strings.HasPrefix(rep.Version, "v1.") {
		t.Fatalf("version %q was not parsed from the real API server", rep.Version)
	}
	if rep.APIEndpoint != "https://127.0.0.1:6443" {
		t.Fatalf("endpoint = %q", rep.APIEndpoint)
	}
}
