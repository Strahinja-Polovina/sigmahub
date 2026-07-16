package reconciler

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// ACMEConfig is the control-plane's ACME issuance configuration, rendered into
// every proxy.traefik op. CADirURL empty means Let's Encrypt production; the
// staging/Pebble URL is injected for the e2e environment. Email is the ACME
// account contact.
type ACMEConfig struct {
	Email    string
	CADirURL string
}

// traefikCertResolver is the fixed resolver name both the static config (agent)
// and the router labels (here) reference.
const traefikCertResolver = "le"

// traefikOpSpec is the wire contract for the proxy.traefik op. The agent renders
// Traefik's static config from it: the web/websecure entrypoints, an ACME
// resolver (challenge type + CA directory + account email), and a persistent
// acme store. ChallengeType is 'http' or 'tls-alpn' in P1-8 ('dns' is a P1-12
// hook). The op is idempotent: the agent reuses the persisted acme store, so a
// re-apply issues no new ACME order.
type traefikOpSpec struct {
	ServerID      string `json:"serverId"`
	CertResolver  string `json:"certResolver"`
	ChallengeType string `json:"challengeType"`
	ACMEEmail     string `json:"acmeEmail,omitempty"`
	ACMECADirURL  string `json:"acmeCaDirUrl,omitempty"`
}

// renderTraefikOp emits the proxy.traefik op for a proxy-role server. A server
// runs ONE ACME resolver, so the challenge type is a per-server property derived
// from its domains: TLS-ALPN-01 only when EVERY attached domain requests it (so
// an HTTP-01 domain is never silently broken by a resolver it can't use);
// otherwise HTTP-01, the safe default. DNS-01 remains a P1-12 hook.
func renderTraefikOp(serverID string, cfg ACMEConfig, domains []store.Domain) dsd.Op {
	challenge := "http"
	if len(domains) > 0 {
		allTLSALPN := true
		for _, d := range domains {
			if d.ChallengeType != "tls-alpn" {
				allTLSALPN = false
				break
			}
		}
		if allTLSALPN {
			challenge = "tls-alpn"
		}
	}
	spec, _ := json.Marshal(traefikOpSpec{
		ServerID:      serverID,
		CertResolver:  traefikCertResolver,
		ChallengeType: challenge,
		ACMEEmail:     cfg.Email,
		ACMECADirURL:  cfg.CADirURL,
	})
	return dsd.Op{ID: "proxy:traefik:" + serverID, Kind: dsd.KindProxyTraefik, Spec: spec}
}

// traefikLabels renders the Docker labels that make Traefik route the resource's
// domains to it over HTTPS with an auto-issued certificate. port is the
// container port Traefik connects to on the shared project network. Returns nil
// when the resource has no domains (no router labels — the container is still
// reachable internally, and gains labels only once a domain is attached).
// traefikHealthLabels makes Traefik health-check each backend so it withholds
// traffic from a new, not-yet-healthy generation — the load-balancer-level gate
// for a git-deployed app whose two generations share the same router labels
// during a blue-green swap. Only emitted for an HTTP probe: Traefik's health check
// is an HTTP GET, so a TCP-only probe has no LB equivalent and the agent's own
// health gate remains the guard.
func traefikHealthLabels(resourceID string, h healthProbe) map[string]string {
	if h.Type != "http" || h.Path == "" {
		return nil
	}
	svc := dsd.TraefikRouterName(resourceID)
	prefix := "traefik.http.services." + svc + ".loadbalancer.healthcheck."
	interval := h.IntervalSec
	if interval <= 0 {
		interval = 3
	}
	labels := map[string]string{
		prefix + "path":     h.Path,
		prefix + "interval": strconv.Itoa(interval) + "s",
	}
	if h.Port > 0 {
		labels[prefix+"port"] = strconv.Itoa(h.Port)
	}
	return labels
}

func traefikLabels(resourceID string, domains []store.Domain, port int) map[string]string {
	if len(domains) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(domains))
	for _, d := range domains {
		hosts = append(hosts, "Host(`"+d.Domain+"`)")
	}
	sort.Strings(hosts) // deterministic rule → deterministic doc hash
	router := dsd.TraefikRouterName(resourceID)
	labels := map[string]string{
		"traefik.enable": "true",
		"traefik.http.routers." + router + ".rule":             strings.Join(hosts, " || "),
		"traefik.http.routers." + router + ".entrypoints":      "websecure",
		"traefik.http.routers." + router + ".tls":              "true",
		"traefik.http.routers." + router + ".tls.certresolver": traefikCertResolver,
	}
	// Only pin the upstream port when the app declares one; otherwise let
	// Traefik auto-detect the container's single exposed port rather than
	// silently defaulting to 80 (which would misroute an app on another port).
	if port > 0 {
		labels["traefik.http.services."+router+".loadbalancer.server.port"] = strconv.Itoa(port)
	}
	return labels
}
