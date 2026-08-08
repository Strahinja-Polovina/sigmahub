package dsd

import (
	"regexp"
	"strings"
	"testing"
)

// rfc1123 is the rule Kubernetes enforces on object names — the SAME expression
// the agent validates with before it writes a manifest. Duplicated here rather
// than shared because the two modules cannot import each other; that duplication
// is exactly why this test exists.
var rfc1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Identifiers are `prefix_hex`, and that underscore made every name the control
// plane rendered illegal — the agent rejected each k8s.apply before writing
// anything, so no cluster workload could ever be deployed. Nothing caught it
// because the agent's own test hand-wrote "api" as the name instead of asking
// the control plane what it would actually send.
func TestRenderedKubernetesNamesAreLegal(t *testing.T) {
	cases := []struct{ resource, service, project string }{
		{"res_0a1b2c3d4e5f6071", "", "prj_9f8e7d6c5b4a3921"},
		{"res_0a1b2c3d4e5f6071", "web", "prj_9f8e7d6c5b4a3921"},
		{"res_0a1b2c3d4e5f6071", "worker_queue", "prj_9f8e7d6c5b4a3921"},
		{"res_0a1b2c3d4e5f6071", "API.Gateway", "prj_9f8e7d6c5b4a3921"},
	}
	for _, c := range cases {
		name := K8sWorkloadName(c.resource, c.service)
		if !rfc1123.MatchString(name) {
			t.Errorf("workload name %q (resource %s, service %q) is not a legal Kubernetes name",
				name, c.resource, c.service)
		}
		if len(name) > k8sNameMax {
			t.Errorf("workload name %q is %d chars, over the %d limit", name, len(name), k8sNameMax)
		}
		ns := K8sNamespace(c.project)
		if !rfc1123.MatchString(ns) {
			t.Errorf("namespace %q is not a legal Kubernetes name", ns)
		}
	}
}

// Truncation alone would collapse two long service names that share a prefix
// onto one Deployment — one service silently overwriting the other.
func TestLongNamesStayDistinct(t *testing.T) {
	long := strings.Repeat("a", 80)
	first := K8sWorkloadName("res_0a1b2c3d4e5f6071", long+"-one")
	second := K8sWorkloadName("res_0a1b2c3d4e5f6071", long+"-two")
	if first == second {
		t.Fatalf("two services collapsed onto the same name: %q", first)
	}
	for _, n := range []string{first, second} {
		if !rfc1123.MatchString(n) || len(n) > k8sNameMax {
			t.Fatalf("truncated name %q (%d chars) is not legal", n, len(n))
		}
	}
}

// A resync must render byte-identical manifests, so the name has to be a pure
// function of its inputs.
func TestK8sNameIsDeterministic(t *testing.T) {
	for i := 0; i < 50; i++ {
		if got := K8sWorkloadName("res_0a1b2c3d4e5f6071", "web"); got != K8sWorkloadName("res_0a1b2c3d4e5f6071", "web") {
			t.Fatalf("name is not deterministic: %q", got)
		}
	}
}

// Nothing legal to build a name from must yield "" so the caller skips the op,
// rather than inventing a name that identifies nothing.
func TestK8sNameRefusesEmptyResult(t *testing.T) {
	if got := K8sName("___"); got != "" {
		t.Fatalf("K8sName(%q) = %q, want empty", "___", got)
	}
}

// An image built on one host and run on another has to name the registry both
// can reach; a bare `sigmahub/...` tag resolves to docker.io under a namespace
// nobody owns, which is a 401 on push and an ImagePullBackOff on the far side.
func TestQualifyImage(t *testing.T) {
	local := PinnedImageTag("res_1", "abc123", "pin1")
	cases := []struct{ repo, want string }{
		{"", local},
		{"ghcr.io/acme", "ghcr.io/acme/res_1:abc123-pin1"},
		{"ghcr.io/acme/", "ghcr.io/acme/res_1:abc123-pin1"},
		{"registry.internal:5000", "registry.internal:5000/res_1:abc123-pin1"},
	}
	for _, c := range cases {
		if got := QualifyImage(c.repo, local); got != c.want {
			t.Errorf("QualifyImage(%q, %q) = %q, want %q", c.repo, local, got, c.want)
		}
	}
}
