package reconciler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestRenderTraefikOpOnlyForProxyRole(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{"image": "nginx", "ports": []map[string]any{{"container": 8080}}})
	specs := []store.ResourceSpec{{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec}}
	acme := ACMEConfig{Email: "ops@example.com", CADirURL: "https://pebble:14000/dir"}

	// Non-proxy server: no traefik op.
	ops, _ := renderOps("srv_np", specs, nil, nil, store.HostHardening{ProxyRole: false}, nil, nil, nil, nil, nil, acme)
	if _, ok := opByID(ops, "proxy:traefik:srv_np"); ok {
		t.Error("non-proxy server must not get a traefik op")
	}

	// Proxy server: traefik op present, carrying the ACME config.
	ops, _ = renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, nil, nil, nil, nil, nil, acme)
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

	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, ACMEConfig{})
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
	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, allAlpn, nil, nil, nil, nil, ACMEConfig{})
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
	ops, _ = renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, mixed, nil, nil, nil, nil, ACMEConfig{})
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
	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, ACMEConfig{})
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
	ops, _ := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, nil, nil, nil, nil, nil, ACMEConfig{})
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
	_, h1 := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, ACMEConfig{Email: "x@y.z"})
	_, h2 := renderOps("srv_p", specs, nil, nil, store.HostHardening{ProxyRole: true}, domains, nil, nil, nil, nil, ACMEConfig{Email: "x@y.z"})
	if h1 != h2 {
		t.Errorf("render not deterministic: %s vs %s", h1, h2)
	}
}
