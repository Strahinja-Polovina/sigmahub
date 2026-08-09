package container

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

func hasArg(args []string, substr string) bool {
	for _, a := range args {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}

func TestTraefikArgsHTTPChallenge(t *testing.T) {
	args := traefikArgs(TraefikSpec{CertResolver: "le", ChallengeType: "http", ACMEEmail: "ops@x.io", ACMECADirURL: "https://pebble:14000/dir"})
	for _, want := range []string{
		"--entrypoints.web.address=:80",
		"--entrypoints.websecure.address=:443",
		"--providers.docker=true",
		"--providers.docker.exposedbydefault=false",
		"certificatesresolvers.le.acme.httpchallenge=true",
		"certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
		"certificatesresolvers.le.acme.email=ops@x.io",
		"certificatesresolvers.le.acme.storage=/acme/acme.json",
		"certificatesresolvers.le.acme.caserver=https://pebble:14000/dir",
	} {
		if !hasArg(args, want) {
			t.Errorf("missing arg %q in %v", want, args)
		}
	}
	if hasArg(args, "tlschallenge") {
		t.Error("http challenge must not enable tlschallenge")
	}
}

func TestTraefikArgsTLSALPN(t *testing.T) {
	args := traefikArgs(TraefikSpec{ChallengeType: "tls-alpn"})
	if !hasArg(args, "acme.tlschallenge=true") {
		t.Errorf("tls-alpn must enable tlschallenge: %v", args)
	}
	if hasArg(args, "httpchallenge") {
		t.Error("tls-alpn must not enable httpchallenge")
	}
}

func TestTraefikArgsNoCAServerForProd(t *testing.T) {
	// Empty CA dir → Let's Encrypt production (no caserver override).
	args := traefikArgs(TraefikSpec{ChallengeType: "http"})
	if hasArg(args, "caserver") {
		t.Errorf("prod must not set a caserver override: %v", args)
	}
}

func TestBuildTraefikCreateBody(t *testing.T) {
	body := buildTraefikCreateBody(TraefikSpec{CertResolver: "le", ChallengeType: "http"})
	if body["Image"] != traefikImage {
		t.Errorf("image = %v", body["Image"])
	}
	labels := body["Labels"].(map[string]string)
	if labels[LabelManaged] != "true" || labels["traefik.enable"] != "false" {
		t.Errorf("labels wrong: %v", labels)
	}
	if labels[traefikSpecHashLabel] == "" {
		t.Error("missing spec-hash label (drift detection)")
	}
	hc := body["HostConfig"].(map[string]any)
	binds := hc["Binds"].([]string)
	var sock, acme bool
	for _, b := range binds {
		if strings.Contains(b, "/var/run/docker.sock") && strings.HasSuffix(b, ":ro") {
			sock = true
		}
		if strings.HasPrefix(b, traefikACMEVolume+":") {
			acme = true
		}
	}
	if !sock {
		t.Errorf("docker socket must be mounted read-only: %v", binds)
	}
	if !acme {
		t.Errorf("acme volume must be mounted: %v", binds)
	}
	pb := hc["PortBindings"].(map[string]any)
	if _, ok := pb["80/tcp"]; !ok {
		t.Error("port 80 not published")
	}
	if _, ok := pb["443/tcp"]; !ok {
		t.Error("port 443 not published")
	}
}

// TestTraefikSpecHashChangesOnConfig proves a config change recreates the proxy.
func TestTraefikSpecHashChangesOnConfig(t *testing.T) {
	a := TraefikSpec{ChallengeType: "http", ACMEEmail: "a@x.io"}.hash()
	b := TraefikSpec{ChallengeType: "http", ACMEEmail: "b@x.io"}.hash()
	if a == b {
		t.Error("changing the ACME email must change the spec hash")
	}
	if a != (TraefikSpec{ChallengeType: "http", ACMEEmail: "a@x.io"}).hash() {
		t.Error("hash must be deterministic")
	}
}

// The drift signal must cover the IMAGE, not just the op spec. It did not, and
// the consequence was that shipping a new Traefik in the agent changed nothing
// on a host that already had one: opProxyTraefik pulls traefikImage, compares
// the spec hash, finds it equal and returns. A proxy pinned to a version that
// cannot talk to the host's Docker daemon would have stayed pinned there
// through every upgrade, with the fix sitting unused in the pulled image.
func TestProxyDriftHashCoversTheImage(t *testing.T) {
	spec := TraefikSpec{ChallengeType: "http", ACMEEmail: "a@x.io"}
	if proxyDriftHash(spec) == spec.hash() {
		t.Error("drift hash must not be the bare spec hash — the image has to be in it")
	}
	if proxyDriftHash(spec) != proxyDriftHash(spec) {
		t.Error("drift hash must be deterministic")
	}
	// The label written at create time and the value compared on the next apply
	// have to be the same function, or every apply sees drift and recreates the
	// proxy in a loop — dropping ingress each time.
	body := buildTraefikCreateBody(spec)
	labels, ok := body["Labels"].(map[string]string)
	if !ok {
		t.Fatalf("labels missing from create body: %#v", body["Labels"])
	}
	if labels[traefikSpecHashLabel] != proxyDriftHash(spec) {
		t.Errorf("create-time label %q != drift hash %q",
			labels[traefikSpecHashLabel], proxyDriftHash(spec))
	}
}

// The Docker-provider API floor (Docker Engine 29 rejects the 1.24 client that
// Traefik hardcoded before v3.6.1). Pinned as a test because the failure it
// prevents is invisible: the proxy runs, the apply succeeds, and every domain
// on the host 404s.
func TestTraefikImageMeetsTheDockerAPIFloor(t *testing.T) {
	// Compared as numbers, not as strings: "v3.10.0" sorts BELOW "v3.6.1"
	// lexicographically, so a string compare here would reject the very upgrade
	// it exists to encourage.
	parse := func(image string) [3]int {
		t.Helper()
		_, ver, ok := strings.Cut(image, ":v")
		if !ok {
			t.Fatalf("image %q is not tag-pinned as :v<semver>", image)
		}
		var out [3]int
		for i, part := range strings.SplitN(ver, ".", 3) {
			n, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("image %q has a non-numeric version part %q", image, part)
			}
			out[i] = n
		}
		return out
	}
	got, floor := parse(traefikImage), parse("traefik:v3.6.1")
	for i := range got {
		if got[i] > floor[i] {
			return
		}
		if got[i] < floor[i] {
			t.Fatalf("traefikImage %q predates the Docker-29 API negotiation fix (need >= traefik:v3.6.1)", traefikImage)
		}
	}
}

func TestParseLeaf(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(0x1234abcd),
		Subject:      pkix.Name{CommonName: "app.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	b64 := base64.StdEncoding.EncodeToString(pemBytes)

	serial, expiry := parseLeaf(b64)
	if serial != "1234ABCD" {
		t.Errorf("serial = %q, want 1234ABCD", serial)
	}
	if expiry == nil || !expiry.Equal(notAfter) {
		t.Errorf("expiry = %v, want %v", expiry, notAfter)
	}
}

// TestDesiredNamesKeepsProxy proves GC's keep-set includes the Traefik proxy
// when the document carries a proxy.traefik op — without this, GC (which lists
// every managed container) force-removes the proxy after every apply.
func TestDesiredNamesKeepsProxy(t *testing.T) {
	appSpec, _ := json.Marshal(ContainerSpec{Name: "sigmahub-res_a"})
	doc := dsd.Document{Ops: []dsd.Op{
		{ID: "res:res_a", Kind: KindContainerApply, Spec: appSpec},
		{ID: "proxy:traefik:srv", Kind: KindProxyTraefik, Spec: []byte(`{}`)},
	}}
	names := desiredNames(doc)
	if !names[traefikContainerName] {
		t.Errorf("keep-set must include the Traefik proxy %q; got %v", traefikContainerName, names)
	}
	if !names["sigmahub-res_a"] {
		t.Error("keep-set must still include app containers")
	}

	// With the proxy role cleared (no proxy.traefik op), the proxy is NOT kept —
	// so GC correctly tears it down.
	doc2 := dsd.Document{Ops: []dsd.Op{{ID: "res:res_a", Kind: KindContainerApply, Spec: appSpec}}}
	if desiredNames(doc2)[traefikContainerName] {
		t.Error("without a proxy.traefik op the proxy must not be in the keep-set")
	}
}

func TestTraefikCertReportShape(t *testing.T) {
	// The report marshals to the shape the CP ingest endpoint expects.
	r := DomainCertReport{Domain: "a.example.com", Status: "issued", Serial: "AB"}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"domain":"a.example.com"`) || !strings.Contains(string(b), `"status":"issued"`) {
		t.Errorf("report JSON = %s", b)
	}
}
