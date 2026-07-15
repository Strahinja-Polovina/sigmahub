package container

import (
	"errors"
	"testing"
	"time"
)

func TestPolicyDenies(t *testing.T) {
	base := ContainerSpec{Image: "nginx:1.27", Network: "sigmahub-proj_x"}
	cases := []struct {
		name string
		mut  func(*ContainerSpec)
		rule string
	}{
		{"privileged", func(s *ContainerSpec) { s.Privileged = true }, "privileged"},
		{"host network flag", func(s *ContainerSpec) { s.HostNetwork = true }, "host-network"},
		{"host pid", func(s *ContainerSpec) { s.HostPID = true }, "host-pid"},
		{"host mount", func(s *ContainerSpec) { s.HostMounts = []HostMount{{Source: "/etc", Target: "/etc"}} }, "host-mount"},
		{"network host mode", func(s *ContainerSpec) { s.Network = "host" }, "network"},
		{"network container join", func(s *ContainerSpec) { s.Network = "container:abc123" }, "network"},
		{"network raw ns", func(s *ContainerSpec) { s.Network = "ns:/proc/1/ns/net" }, "network"},
		{"empty network", func(s *ContainerSpec) { s.Network = "" }, "network"},
		{"floating latest", func(s *ContainerSpec) { s.Image = "nginx:latest" }, "image"},
		{"untagged", func(s *ContainerSpec) { s.Image = "nginx" }, "image"},
		{"empty image", func(s *ContainerSpec) { s.Image = "" }, "image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mut(&s)
			err := CheckPolicy(s)
			if err == nil {
				t.Fatalf("expected denial for %s", tc.name)
			}
			var pe *PolicyError
			if !errors.As(err, &pe) {
				t.Fatalf("want *PolicyError, got %T", err)
			}
			if pe.Rule != tc.rule {
				t.Fatalf("rule = %q, want %q", pe.Rule, tc.rule)
			}
		})
	}
}

func TestPolicyAllows(t *testing.T) {
	for _, img := range []string{
		"nginx:1.27",
		"nginxinc/nginx-unprivileged:1.27-alpine",
		"ghcr.io/acme/app:v1.2.3",
		"registry.example.com:5000/team/app:2024-01",
		"nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if err := CheckPolicy(ContainerSpec{Image: img, Network: "sigmahub-proj_x"}); err != nil {
			t.Errorf("image %q rejected: %v", img, err)
		}
	}
	// "none" is a safe non-namespace-sharing mode and is allowed.
	if err := CheckPolicy(ContainerSpec{Image: "nginx:1.27", Network: "none"}); err != nil {
		t.Errorf("network none rejected: %v", err)
	}
}

func TestTerminalRestart(t *testing.T) {
	// A hash-matching stopped container is converged under a terminal policy but
	// is drift under a keep-running policy. The observed container's spec-hash
	// label must match the spec being converged.
	spec := ContainerSpec{Name: "c", Image: "nginx:1.27", Network: "sigmahub-x"}
	hashState := func(s ContainerSpec, running bool) ContainerState {
		return ContainerState{Labels: map[string]string{LabelSpecHash: s.SpecHash()}, Running: running}
	}

	if !converged(spec, hashState(spec, true), true) {
		t.Error("a running hash-matching container must be converged")
	}
	for _, policy := range []string{"no", "on-failure"} {
		s := spec
		s.Restart = policy
		if !converged(s, hashState(s, false), true) {
			t.Errorf("stopped container under %q should be converged", policy)
		}
	}
	for _, policy := range []string{"", "always", "unless-stopped"} {
		s := spec
		s.Restart = policy
		if converged(s, hashState(s, false), true) {
			t.Errorf("stopped container under keep-running policy %q must be drift", policy)
		}
	}
	if converged(spec, ContainerState{}, false) {
		t.Error("absent container is never converged")
	}
	// A hash MISMATCH is never converged, even if running.
	if converged(spec, ContainerState{Labels: map[string]string{LabelSpecHash: "stale"}, Running: true}, true) {
		t.Error("a stale-hash container must not be converged")
	}
}

func TestAllowlistDisabledByDefault(t *testing.T) {
	var a ImageAllowlist // zero value: disabled
	if err := a.Check("anything/unsigned:1.0"); err != nil {
		t.Fatalf("disabled allowlist should allow all, got %v", err)
	}
	a = ImageAllowlist{Enabled: true, Prefixes: []string{"ghcr.io/acme/"}}
	if err := a.Check("ghcr.io/acme/app:1.0"); err != nil {
		t.Fatalf("allowlisted prefix rejected: %v", err)
	}
	if err := a.Check("docker.io/evil/app:1.0"); err == nil {
		t.Fatal("non-allowlisted image accepted while enabled")
	}
}

func TestSpecHashStableAndSensitive(t *testing.T) {
	s := ContainerSpec{ResourceID: "r1", Name: "c", Image: "nginx:1.27", MemoryMB: 256}
	h1 := s.SpecHash()
	if h1 != s.SpecHash() {
		t.Fatal("spec hash not stable across calls")
	}
	s2 := s
	s2.MemoryMB = 512
	if s2.SpecHash() == h1 {
		t.Fatal("spec hash did not change with the spec")
	}
}

func TestSplitImageRef(t *testing.T) {
	cases := []struct{ in, repo, tag string }{
		{"nginx:1.27", "nginx", "1.27"},
		{"nginx", "nginx", "latest"},
		{"registry.example.com:5000/team/app:2024-01", "registry.example.com:5000/team/app", "2024-01"},
		{"nginx@sha256:abcd", "nginx@sha256:abcd", ""},
	}
	for _, c := range cases {
		repo, tag := splitImageRef(c.in)
		if repo != c.repo || tag != c.tag {
			t.Errorf("splitImageRef(%q) = (%q,%q), want (%q,%q)", c.in, repo, tag, c.repo, c.tag)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	base := time.Unix(0, 0)
	now := base
	r := newRateLimiter(2, 1) // burst 2, 1/sec
	r.now = func() time.Time { return now }
	r.lastFill = base // align the bucket clock with the injected clock

	if !r.allow() || !r.allow() {
		t.Fatal("burst of 2 should be allowed")
	}
	if r.allow() {
		t.Fatal("third immediate op should be throttled")
	}
	now = base.Add(1100 * time.Millisecond) // ~1 token refilled
	if !r.allow() {
		t.Fatal("op after refill should be allowed")
	}
}
