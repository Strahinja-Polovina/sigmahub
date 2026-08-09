package container

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// KindProxyTraefik is the op that stands up the Traefik ingress on a proxy-role
// server. Name matches the control plane's dsd.KindProxyTraefik byte-for-byte.
const KindProxyTraefik = "proxy.traefik"

const (
	// traefikImage is a FLOOR, not a preference. Traefik's Docker provider built
	// its API client at a hardcoded 1.24 until the auto-negotiation fix, and
	// Docker Engine 29 dropped support for versions that old. The pairing is
	// silently catastrophic: the daemon rejects every provider call, so Traefik
	// discovers no containers, registers no routers, and answers 404 for every
	// domain on the host — while the container itself is up, healthy, and
	// reporting a successful apply. The only evidence is in Traefik's own log
	// ("client version 1.24 is too old"), which nothing surfaces.
	//
	// v3.6.1 is the first release carrying the negotiation fix. Do not lower it.
	traefikImage         = "traefik:v3.6.1"
	traefikContainerName = "sigmahub-traefik"
	traefikACMEVolume    = "sigmahub-traefik-acme"
	traefikSpecHashLabel = "sigmahub.traefikSpecHash"
)

// TraefikSpec is the payload of a proxy.traefik op (mirrors the control plane's
// traefikOpSpec). ACMECADirURL empty means Let's Encrypt production.
type TraefikSpec struct {
	ServerID      string `json:"serverId"`
	CertResolver  string `json:"certResolver"`
	ChallengeType string `json:"challengeType"` // "http" | "tls-alpn"
	ACMEEmail     string `json:"acmeEmail"`
	ACMECADirURL  string `json:"acmeCaDirUrl"`
}

func (s TraefikSpec) hash() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// proxyDriftHash is what the proxy container is compared against on every apply:
// the op's spec AND the image this agent pins.
//
// The image used to be outside the signal, which made the version pin advisory.
// opProxyTraefik pulls traefikImage and then compares only spec.hash(), so an
// agent shipping a NEW Traefik pulled it and left the OLD container running —
// for as long as nobody edited an unrelated ACME setting. That is the worst
// shape a version pin can have: upgrading the agent to fix a broken proxy would
// have fixed nothing, and the fix would have looked deployed.
func proxyDriftHash(spec TraefikSpec) string {
	sum := sha256.Sum256([]byte(spec.hash() + "|" + traefikImage))
	return hex.EncodeToString(sum[:8])
}

// traefikArgs renders Traefik's static config as CLI flags. Deterministic order
// so the container's spec hash is stable across resyncs.
func traefikArgs(spec TraefikSpec) []string {
	resolver := spec.CertResolver
	if resolver == "" {
		resolver = "le"
	}
	args := []string{
		"--entrypoints.web.address=:80",
		"--entrypoints.websecure.address=:443",
		// Redirect plain HTTP to HTTPS. Traefik still serves the ACME HTTP-01
		// challenge on :80 before applying the redirect.
		"--entrypoints.web.http.redirections.entrypoint.to=websecure",
		"--entrypoints.web.http.redirections.entrypoint.scheme=https",
		"--providers.docker=true",
		"--providers.docker.exposedbydefault=false",
		fmt.Sprintf("--certificatesresolvers.%s.acme.storage=/acme/acme.json", resolver),
	}
	if spec.ACMEEmail != "" {
		args = append(args, fmt.Sprintf("--certificatesresolvers.%s.acme.email=%s", resolver, spec.ACMEEmail))
	}
	switch spec.ChallengeType {
	case "tls-alpn":
		args = append(args, fmt.Sprintf("--certificatesresolvers.%s.acme.tlschallenge=true", resolver))
	default: // http (HTTP-01)
		args = append(args,
			fmt.Sprintf("--certificatesresolvers.%s.acme.httpchallenge=true", resolver),
			fmt.Sprintf("--certificatesresolvers.%s.acme.httpchallenge.entrypoint=web", resolver))
	}
	if spec.ACMECADirURL != "" {
		args = append(args, fmt.Sprintf("--certificatesresolvers.%s.acme.caserver=%s", resolver, spec.ACMECADirURL))
	}
	return args
}

// buildTraefikCreateBody assembles the Docker create body for the proxy. The
// container carries the same isolation defaults as workloads (no-new-privileges,
// apparmor), publishes 80/443, and mounts a persistent ACME volume (the
// idempotency mechanism: the held certificate survives a recreate, so no new
// ACME order is placed).
//
// The Docker socket is mounted for the label provider. The `:ro` bind only makes
// the socket FILE read-only — it does NOT restrict the Docker API, so the proxy
// still has effective host-root through the daemon. This is inherent to Traefik's
// docker provider; a socket-proxy that exposes a read-only API subset is the
// hardening follow-up (tracked for a later phase), not shipped here.
func buildTraefikCreateBody(spec TraefikSpec) map[string]any {
	labels := map[string]string{
		LabelManaged:         "true",
		LabelResourceID:      "sigmahub-traefik",
		traefikSpecHashLabel: proxyDriftHash(spec),
		// Never route to the proxy itself.
		"traefik.enable": "false",
	}
	exposed := map[string]any{"80/tcp": map[string]any{}, "443/tcp": map[string]any{}}
	portBindings := map[string]any{
		"80/tcp":  []map[string]string{{"HostPort": "80"}},
		"443/tcp": []map[string]string{{"HostPort": "443"}},
	}
	hostConfig := map[string]any{
		"SecurityOpt":   []string{"no-new-privileges:true", "apparmor=docker-default"},
		"Privileged":    false,
		"RestartPolicy": map[string]any{"Name": "always"},
		"PortBindings":  portBindings,
		"Binds": []string{
			"/var/run/docker.sock:/var/run/docker.sock:ro",
			traefikACMEVolume + ":/acme:rw",
		},
	}
	return map[string]any{
		"Image":        traefikImage,
		"Labels":       labels,
		"Cmd":          traefikArgs(spec),
		"ExposedPorts": exposed,
		"HostConfig":   hostConfig,
	}
}

// opProxyTraefik ensures the Traefik proxy is running with the desired static
// config, then attaches it to every managed project network so it can reach the
// apps it fronts. Idempotent: an unchanged spec leaves the running container (and
// its persisted ACME store) untouched.
func (d *Driver) opProxyTraefik(ctx context.Context, op dsd.Op) error {
	var spec TraefikSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode traefik spec: %w", err)
	}

	// Persistent ACME store — created once, reused across recreates.
	if exists, err := d.docker.VolumeExists(ctx, traefikACMEVolume); err != nil {
		return err
	} else if !exists {
		if err := d.docker.VolumeCreate(ctx, traefikACMEVolume, map[string]string{LabelManaged: "true"}); err != nil {
			return fmt.Errorf("create acme volume: %w", err)
		}
	}

	if err := d.docker.ImagePull(ctx, traefikImage); err != nil {
		return fmt.Errorf("pull traefik: %w", err)
	}

	cur, exists, err := d.docker.ContainerInspect(ctx, traefikContainerName)
	if err != nil {
		return err
	}
	want := proxyDriftHash(spec)
	if exists && cur.Labels[traefikSpecHashLabel] == want {
		// Config unchanged: make sure it is running, then (re)attach networks.
		if !cur.Running {
			if err := d.docker.ContainerStart(ctx, cur.ID); err != nil {
				return err
			}
		}
		return d.attachProxyNetworks(ctx)
	}

	// Changed or absent: recreate.
	if exists {
		if err := d.docker.ContainerStop(ctx, cur.ID, 10*time.Second); err != nil {
			return err
		}
		if err := d.docker.ContainerRemove(ctx, cur.ID, true); err != nil {
			return err
		}
	}
	id, err := d.docker.ContainerCreate(ctx, traefikContainerName, buildTraefikCreateBody(spec))
	if err != nil {
		return fmt.Errorf("create traefik: %w", err)
	}
	if err := d.docker.ContainerStart(ctx, id); err != nil {
		return err
	}
	return d.attachProxyNetworks(ctx)
}

// attachProxyNetworks connects Traefik to every managed project network. Running
// on every apply keeps the proxy reachable to apps in projects created after it
// started.
func (d *Driver) attachProxyNetworks(ctx context.Context) error {
	nets, err := d.docker.ManagedNetworks(ctx)
	if err != nil {
		return err
	}
	for _, n := range nets {
		if err := d.docker.NetworkConnect(ctx, n, traefikContainerName); err != nil {
			return fmt.Errorf("connect traefik to %s: %w", n, err)
		}
	}
	return nil
}

// DomainCertReport is one domain's certificate state, read from Traefik's ACME
// store for reporting back to the control plane.
type DomainCertReport struct {
	Domain    string     `json:"domain"`
	Status    string     `json:"status"`
	Serial    string     `json:"serial,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// acmeStore is the subset of Traefik's acme.json the reader needs.
type acmeStore map[string]struct {
	Certificates []struct {
		Domain struct {
			Main string   `json:"main"`
			SANs []string `json:"sans"`
		} `json:"domain"`
		Certificate string `json:"certificate"` // base64 PEM chain
	} `json:"Certificates"`
}

// TraefikCertStatus reads Traefik's ACME store from the persistent volume and
// reports the issued certificates (serial + expiry parsed from the leaf). A
// missing/empty store yields no reports (nothing issued yet), not an error.
func (d *Driver) TraefikCertStatus(ctx context.Context) ([]DomainCertReport, error) {
	mount, err := d.docker.VolumeMountpoint(ctx, traefikACMEVolume)
	if err != nil {
		return nil, nil // no acme volume yet
	}
	raw, err := os.ReadFile(filepath.Join(mount, "acme.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var store acmeStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, fmt.Errorf("parse acme.json: %w", err)
	}
	var out []DomainCertReport
	seen := map[string]bool{}
	for _, resolver := range store {
		for _, c := range resolver.Certificates {
			domains := append([]string{c.Domain.Main}, c.Domain.SANs...)
			serial, expiry := parseLeaf(c.Certificate)
			for _, dom := range domains {
				dom = strings.ToLower(strings.TrimSpace(dom))
				if dom == "" || seen[dom] {
					continue
				}
				seen[dom] = true
				out = append(out, DomainCertReport{Domain: dom, Status: "issued", Serial: serial, ExpiresAt: expiry})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

// parseLeaf pulls the serial (hex) and NotAfter from the first (leaf) certificate
// in a base64-encoded PEM chain. Best-effort: parse failures yield empty values.
func parseLeaf(b64 string) (serial string, expiry *time.Time) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil
	}
	block, _ := pem.Decode(der)
	if block == nil {
		return "", nil
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", nil
	}
	exp := crt.NotAfter
	return strings.ToUpper(crt.SerialNumber.Text(16)), &exp
}
