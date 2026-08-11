package reconciler

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

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

// traefikHealthLabels makes Traefik health-check the backend so it withholds
// traffic from a container that is up but not yet serving. Only emitted for an
// HTTP probe: Traefik's health check is an HTTP GET, so a TCP-only probe has no
// LB equivalent and the agent's own health gate remains the guard. svc is the
// service name the labels belong to — generation-scoped for a blue-green app, so
// the check applies to that generation's own single-server service.
func traefikHealthLabels(svc string, h healthProbe) map[string]string {
	if h.Type != "http" || h.Path == "" {
		return nil
	}
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

// blueGreenRouting scopes a resource's Traefik labels to one generation of a
// blue-green deploy. Zero value (Generation == "") renders the plain
// resource-scoped router used by resources that are replaced in place.
type blueGreenRouting struct {
	Generation string
	// Priority makes the OLDER generation win while both exist. Two routers with
	// the same rule are resolved by priority (highest first), so the incoming
	// generation is rendered with a strictly LOWER priority than its predecessor:
	// it is present in Traefik's config from the moment it starts but never
	// matches a request while the previous generation is still up. When the agent
	// drains the old container its router disappears and the new one — now the
	// only match — takes over. A failed health gate removes the new container
	// having served nothing.
	Priority int
}

// routerEpoch is the base for generationRouterPriority: seconds since
// 2020-01-01T00:00:00Z, subtracted from 1<<30 so the priority is a positive
// int32 that decreases monotonically with deployment time. Valid until the
// subtraction would go negative (year 2054); Traefik compares priorities as
// plain ints, and every explicit value here far exceeds the default (rule
// length), so these routers always order among themselves.
const (
	routerEpoch        int64 = 1577836800 // 2020-01-01T00:00:00Z
	routerPriorityBase int64 = 1 << 30
)

// generationRouterPriority maps a deployment's creation time to a Traefik router
// priority that is lower for newer deployments. It is a pure function of stored
// data (never of wall-clock at render time), so a resync re-renders byte-identical
// labels and the DSD hash stays stable.
//
// Two deployments of the same resource created within the same second tie, which
// degrades to the pre-SIGMA-164 behaviour (Traefik picks one) for that window —
// a deploy takes minutes, so this is a theoretical case, not a practical one.
func generationRouterPriority(createdAt time.Time) int {
	if createdAt.IsZero() {
		return int(routerPriorityBase)
	}
	p := routerPriorityBase - (createdAt.Unix() - routerEpoch)
	if p < 1 {
		p = 1
	}
	if p > routerPriorityBase {
		p = routerPriorityBase
	}
	return int(p)
}

// traefikLabels renders the Docker labels that make Traefik route the resource's
// domains to it over HTTPS with an auto-issued certificate. port is the
// container port Traefik connects to on the shared project network. Returns nil
// when the resource has no domains (no router labels — the container is still
// reachable internally, and gains labels only once a domain is attached).
//
// bg scopes the router and service to a single generation of a blue-green
// deploy. This is what keeps live traffic off a not-yet-healthy container: with
// a shared service name Traefik merged both generations into one weighted
// service and started round-robining onto the new container the moment it came
// up, well before the agent's health gate ran (SIGMA-164).
//
// publicHost is the hostname SigmaHub gives the resource itself (SIGMA-351). It
// is routed ALONGSIDE any custom domains, never instead of them: a customer who
// later attaches their own domain keeps the SigmaHub URL working, so links they
// shared while setting DNS up do not rot. It is empty only when the deployment
// can offer none — no CP_APPS_DOMAIN and no reachable public address on the host
// — and a resource with neither that nor a custom domain still gets no router,
// which is the honest answer: there is nowhere to route from.
func traefikLabels(resourceID string, domains []store.Domain, publicHost string, port int, bg blueGreenRouting) map[string]string {
	if len(domains) == 0 && publicHost == "" {
		return nil
	}
	hosts := make([]string, 0, len(domains)+1)
	for _, d := range domains {
		hosts = append(hosts, "Host(`"+d.Domain+"`)")
	}
	if publicHost != "" {
		hosts = append(hosts, "Host(`"+publicHost+"`)")
	}
	sort.Strings(hosts) // deterministic rule → deterministic doc hash
	router := dsd.TraefikGenerationRouterName(resourceID, bg.Generation)
	labels := map[string]string{
		"traefik.enable": "true",
		"traefik.http.routers." + router + ".rule":             strings.Join(hosts, " || "),
		"traefik.http.routers." + router + ".entrypoints":      "websecure",
		"traefik.http.routers." + router + ".tls":              "true",
		"traefik.http.routers." + router + ".tls.certresolver": traefikCertResolver,
	}
	if bg.Generation != "" && bg.Priority > 0 {
		labels["traefik.http.routers."+router+".priority"] = strconv.Itoa(bg.Priority)
	}
	// Only pin the upstream port when the app declares one; otherwise let
	// Traefik auto-detect the container's single exposed port rather than
	// silently defaulting to 80 (which would misroute an app on another port).
	//
	// The explicit router→service binding is emitted ONLY together with the
	// service definition it names. Naming a service that no label defines would
	// dangle: with no service labels at all, Traefik's docker provider
	// auto-creates the service under a name derived from the CONTAINER, and a
	// router pointing at a name nothing defines serves 404 instead of the app.
	// The auto-created name is per-container and container names are already
	// generation-suffixed, so a portless app still gets one service per
	// generation — which is all SIGMA-164 needs.
	if port > 0 {
		labels["traefik.http.services."+router+".loadbalancer.server.port"] = strconv.Itoa(port)
		labels["traefik.http.routers."+router+".service"] = router
	}
	return labels
}
