package reconciler

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestRenderTraefikOpOnlyForProxyRole(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{"image": "nginx", "ports": []map[string]any{{"container": 8080}}})
	specs := []store.ResourceSpec{{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec}}
	acme := ACMEConfig{Email: "ops@example.com", CADirURL: "https://pebble:14000/dir"}

	// Non-proxy server: no traefik op.
	ops, _ := renderOps("srv_np", specs, nil, nil, store.HostHardening{ProxyRole: false}, nil, nil, nil, nil, nil, nil, acme)
	if _, ok := opByID(ops, "proxy:traefik:srv_np"); ok {
		t.Error("non-proxy server must not get a traefik op")
	}

	// Proxy server: traefik op present, carrying the ACME config.
	ops, _ = renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, nil, nil, nil, nil, nil, nil, acme)
	op, ok := opByID(ops, "proxy:traefik:srv_p")
	if !ok {
		t.Fatal("proxy-role server must get a traefik op")
	}
	if op.Kind != dsd.KindProxyTraefik {
		t.Fatalf("op kind = %q", op.Kind)
	}
	var ts traefikOpSpec
	if err := json.Unmarshal(op.Spec, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.ACMEEmail != "ops@example.com" || ts.ACMECADirURL != "https://pebble:14000/dir" || ts.CertResolver != "le" {
		t.Errorf("traefik spec = %+v", ts)
	}
}

func TestRenderAppLabelsWhenDomainAttached(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{"image": "nginx", "ports": []map[string]any{{"container": 8080}}})
	specs := []store.ResourceSpec{{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec}}
	domains := map[string][]store.Domain{
		"res_a": {{Domain: "app.example.com"}, {Domain: "www.example.com"}},
	}

	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, nil, ACMEConfig{})
	ctr, ok := opByID(ops, "res:res_a")
	if !ok {
		t.Fatal("missing container op")
	}
	var cs containerOpSpec
	if err := json.Unmarshal(ctr.Spec, &cs); err != nil {
		t.Fatal(err)
	}
	router := dsd.TraefikRouterName("res_a")
	if cs.Labels["traefik.enable"] != "true" {
		t.Errorf("missing traefik.enable label: %v", cs.Labels)
	}
	rule := cs.Labels["traefik.http.routers."+router+".rule"]
	if !strings.Contains(rule, "Host(`app.example.com`)") || !strings.Contains(rule, "Host(`www.example.com`)") {
		t.Errorf("router rule = %q, want both hosts", rule)
	}
	if cs.Labels["traefik.http.routers."+router+".tls.certresolver"] != "le" {
		t.Errorf("missing certresolver label: %v", cs.Labels)
	}
	if cs.Labels["traefik.http.services."+router+".loadbalancer.server.port"] != "8080" {
		t.Errorf("loadbalancer port label wrong: %v", cs.Labels)
	}
}

func TestTraefikChallengeTypeFromDomains(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{"image": "nginx", "ports": []map[string]any{{"container": 8080}}})
	specs := []store.ResourceSpec{{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec}}

	// Every domain requests tls-alpn → resolver uses tls-alpn.
	allAlpn := map[string][]store.Domain{"res_a": {
		{Domain: "a.example.com", ChallengeType: "tls-alpn"},
		{Domain: "b.example.com", ChallengeType: "tls-alpn"},
	}}
	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, allAlpn, nil, nil, nil, nil, nil, ACMEConfig{})
	op, _ := opByID(ops, "proxy:traefik:srv_p")
	var ts traefikOpSpec
	_ = json.Unmarshal(op.Spec, &ts)
	if ts.ChallengeType != "tls-alpn" {
		t.Errorf("all-tls-alpn domains → challenge %q, want tls-alpn", ts.ChallengeType)
	}

	// A mix → fall back to http (never silently break an http-01 domain).
	mixed := map[string][]store.Domain{"res_a": {
		{Domain: "a.example.com", ChallengeType: "tls-alpn"},
		{Domain: "b.example.com", ChallengeType: "http"},
	}}
	ops, _ = renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, mixed, nil, nil, nil, nil, nil, ACMEConfig{})
	op, _ = opByID(ops, "proxy:traefik:srv_p")
	_ = json.Unmarshal(op.Spec, &ts)
	if ts.ChallengeType != "http" {
		t.Errorf("mixed challenge domains → %q, want http", ts.ChallengeType)
	}
}

func TestRenderNoPortLabelWhenAppDeclaresNone(t *testing.T) {
	// App with no declared ports + a domain → no loadbalancer port label (Traefik
	// auto-detects) instead of a misleading default of 80.
	spec, _ := json.Marshal(map[string]any{"image": "nginx"})
	specs := []store.ResourceSpec{{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec}}
	domains := map[string][]store.Domain{"res_a": {{Domain: "app.example.com"}}}
	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, nil, ACMEConfig{})
	ctr, _ := opByID(ops, "res:res_a")
	var cs containerOpSpec
	_ = json.Unmarshal(ctr.Spec, &cs)
	router := dsd.TraefikRouterName("res_a")
	if _, ok := cs.Labels["traefik.http.services."+router+".loadbalancer.server.port"]; ok {
		t.Errorf("portless app must not pin a loadbalancer port: %v", cs.Labels)
	}
	if cs.Labels["traefik.enable"] != "true" {
		t.Error("router should still be enabled")
	}
}

func TestRenderNoLabelsWithoutDomain(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{"image": "nginx", "ports": []map[string]any{{"container": 8080}}})
	specs := []store.ResourceSpec{{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec}}
	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, nil, nil, nil, nil, nil, nil, ACMEConfig{})
	ctr, _ := opByID(ops, "res:res_a")
	var cs containerOpSpec
	_ = json.Unmarshal(ctr.Spec, &cs)
	if cs.Labels != nil {
		t.Errorf("a resource with no domain must have no router labels, got %v", cs.Labels)
	}
}

// TestTraefikDeterministicHash proves the render is stable (same inputs → same
// hash), so a resync doesn't churn the DSD version.
func TestTraefikDeterministicHash(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{"image": "nginx", "ports": []map[string]any{{"container": 8080}}})
	specs := []store.ResourceSpec{{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec}}
	domains := map[string][]store.Domain{"res_a": {{Domain: "b.example.com"}, {Domain: "a.example.com"}}}
	_, h1 := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, nil, ACMEConfig{Email: "x@y.z"})
	_, h2 := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, nil, ACMEConfig{Email: "x@y.z"})
	if h1 != h2 {
		t.Errorf("render not deterministic: %s vs %s", h1, h2)
	}
}

// TestBlueGreenRoutersAreGenerationScoped is the SIGMA-164 regression: two
// generations of a blue-green app must NOT share a Traefik router or service
// name. Sharing made Traefik merge them into one weighted service, so the new
// container took live traffic from the moment it started — throughout the whole
// health gate, and even when the gate went on to fail. Each generation now owns
// its router and its single-server service, and the newer generation is rendered
// at a strictly LOWER priority so the incumbent keeps serving until the agent
// drains it.
func TestBlueGreenRoutersAreGenerationScoped(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"ports": []map[string]any{{"container": 3000}}})
	rs := store.ResourceSpec{ResourceID: "res_a", ProjectID: "proj_a", Kind: "app", Spec: raw}
	domains := []store.Domain{{Domain: "app.example.com"}}

	labelsFor := func(deploymentID, sha string, createdAt time.Time) (map[string]string, string) {
		target := store.DeployTarget{
			DeploymentID: deploymentID, ResourceID: "res_a", ProjectID: "proj_a", Provider: "github",
			RepoFullName: "acme/app", Ref: "refs/heads/main", SHA: sha, ConfigHash: "cfg",
			Trigger: "git", CreatedAt: createdAt,
		}
		ops, _, ok := renderDeployOps(rs, nil, domains, target, "")
		if !ok {
			t.Fatal("render should succeed")
		}
		op, ok := opByID(ops, "res:res_a")
		if !ok {
			t.Fatalf("missing rollout op; ops=%v", opIDs(ops))
		}
		var ro rolloutOpSpec
		if err := json.Unmarshal(op.Spec, &ro); err != nil {
			t.Fatal(err)
		}
		return ro.Container.Labels, ro.Generation
	}

	older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	oldLabels, oldGen := labelsFor("dep_1", "aaaaaaaaaa", older)
	newLabels, newGen := labelsFor("dep_2", "bbbbbbbbbb", older.Add(time.Hour))

	if oldGen == newGen {
		t.Fatalf("two deployments produced the same generation %q", oldGen)
	}
	oldRouter := dsd.TraefikGenerationRouterName("res_a", oldGen)
	newRouter := dsd.TraefikGenerationRouterName("res_a", newGen)
	if oldRouter == newRouter {
		t.Fatalf("generations share a router name: %q", oldRouter)
	}

	// Each router must point at its OWN service — otherwise the two containers
	// still merge into one load-balanced service.
	for router, labels := range map[string]map[string]string{oldRouter: oldLabels, newRouter: newLabels} {
		if got := labels["traefik.http.routers."+router+".service"]; got != router {
			t.Errorf("router %q → service %q, want its own service", router, got)
		}
		if got := labels["traefik.http.services."+router+".loadbalancer.server.port"]; got != "3000" {
			t.Errorf("router %q loadbalancer port = %q, want 3000", router, got)
		}
		if !strings.Contains(labels["traefik.http.routers."+router+".rule"], "Host(`app.example.com`)") {
			t.Errorf("router %q lost the host rule: %v", router, labels)
		}
	}

	// The incoming generation must lose the priority contest against the one it
	// is replacing, so it matches nothing until the old container is drained.
	oldPrio := oldLabels["traefik.http.routers."+oldRouter+".priority"]
	newPrio := newLabels["traefik.http.routers."+newRouter+".priority"]
	if oldPrio == "" || newPrio == "" {
		t.Fatalf("blue-green routers must carry an explicit priority: old=%q new=%q", oldPrio, newPrio)
	}
	op, np := mustAtoi(t, oldPrio), mustAtoi(t, newPrio)
	if np >= op {
		t.Errorf("new generation priority %d must be lower than the old %d", np, op)
	}
	if np <= 0 {
		t.Errorf("priority %d must stay positive", np)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("priority %q is not an int: %v", s, err)
	}
	return n
}

// TestRegistryAppKeepsStableRouter guards the other half: an app replaced in
// place has exactly one container, so its router must stay on the plain
// resource-scoped name (a generation suffix would move the router — and its
// issued certificate — on every apply).
func TestRegistryAppKeepsStableRouter(t *testing.T) {
	labels := traefikLabels("res_a", []store.Domain{{Domain: "app.example.com"}}, 8080, blueGreenRouting{})
	router := dsd.TraefikRouterName("res_a")
	if _, ok := labels["traefik.http.routers."+router+".rule"]; !ok {
		t.Fatalf("expected resource-scoped router %q: %v", router, labels)
	}
	if _, ok := labels["traefik.http.routers."+router+".priority"]; ok {
		t.Errorf("an in-place app needs no priority: %v", labels)
	}
}

// TestGenerationRouterPriorityMonotonic pins the ordering property the swap
// depends on, including the zero-time fallback for a target with no stored
// timestamp.
func TestGenerationRouterPriorityMonotonic(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a := generationRouterPriority(base)
	b := generationRouterPriority(base.Add(time.Second))
	if b >= a {
		t.Errorf("later deployment priority %d must be below earlier %d", b, a)
	}
	if a <= 0 || a > 1<<30 {
		t.Errorf("priority %d out of range", a)
	}
	// A zero timestamp (no stored created_at) sorts as the OLDEST, so a target we
	// cannot date never displaces a dated incumbent by accident.
	if z := generationRouterPriority(time.Time{}); z < a {
		t.Errorf("zero-time priority %d must not sort below a real one %d", z, a)
	}
}
