package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// The installer routes are the only unauthenticated read path in the control
// plane that holds a credential to a private repository, so these tests are
// mostly about what the routes REFUSE. Everything runs against an httptest fake
// of GitHub — both shapes of it, the browser download URL and the REST API,
// because the proxy uses a different one depending on whether a token is
// configured and each has its own way of going wrong.

const (
	testReleaseRepo    = "acme/sigmahub"
	testReleaseVersion = "v0.3.0"
	// A value that must never appear in a response, an error body or a header.
	testReleaseToken = "ghp_the_release_credential"
)

type recordedRequest struct {
	method string
	path   string
	auth   string
	accept string
}

type fakeAsset struct {
	tag  string
	name string
	body []byte
	// declared is the size the release listing reports, when it should differ
	// from len(body) — an upstream's declaration is not a guarantee and the
	// proxy has to be tested against one that lies.
	declared int64
	// chunked serves the body without a Content-Length, which is what forces
	// the streaming half of the size cap to be the thing under test.
	chunked bool
}

// fakeGitHub answers both URL shapes the proxy knows, and records every request
// so a test can assert that a refusal happened BEFORE any outbound call.
type fakeGitHub struct {
	mu          sync.Mutex
	assets      []fakeAsset // index+1 is the asset id, mirroring GitHub's numeric ids
	forceStatus int         // when set, every request answers this instead
	requests    []recordedRequest
}

func (g *fakeGitHub) add(a fakeAsset) { g.assets = append(g.assets, a) }

func (g *fakeGitHub) seen() []recordedRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]recordedRequest, len(g.requests))
	copy(out, g.requests)
	return out
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, recordedRequest{
		method: r.Method, path: r.URL.Path,
		auth: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"),
	})
	forced := g.forceStatus
	g.mu.Unlock()

	if forced != 0 {
		// GitHub's own error bodies are JSON; serving one here is what proves
		// the proxy never forwards an upstream body into a root shell.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(forced)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/repos/") {
		g.serveAPI(w, r)
		return
	}
	g.serveDownload(w, r)
}

func (g *fakeGitHub) serveAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if len(parts) != 5 || parts[0]+"/"+parts[1] != testReleaseRepo || parts[2] != "releases" {
		http.NotFound(w, r)
		return
	}
	switch parts[3] {
	case "tags":
		var listing struct {
			Assets []releaseAsset `json:"assets"`
		}
		for i, a := range g.assets {
			if a.tag != parts[4] {
				continue
			}
			size := a.declared
			if size == 0 {
				size = int64(len(a.body))
			}
			listing.Assets = append(listing.Assets, releaseAsset{
				ID: int64(i + 1), Name: a.name, Size: size,
			})
		}
		if len(listing.Assets) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listing)
	case "assets":
		var id int
		if _, err := fmt.Sscanf(parts[4], "%d", &id); err != nil || id < 1 || id > len(g.assets) {
			http.NotFound(w, r)
			return
		}
		writeFakeAsset(w, g.assets[id-1])
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) serveDownload(w http.ResponseWriter, r *http.Request) {
	// /acme/sigmahub/releases/download/v0.3.0/checksums.txt
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 6 || parts[0]+"/"+parts[1] != testReleaseRepo ||
		parts[2] != "releases" || parts[3] != "download" {
		http.NotFound(w, r)
		return
	}
	for _, a := range g.assets {
		if a.tag == parts[4] && a.name == parts[5] {
			writeFakeAsset(w, a)
			return
		}
	}
	http.NotFound(w, r)
}

func writeFakeAsset(w http.ResponseWriter, a fakeAsset) {
	w.Header().Set("Content-Type", "application/octet-stream")
	if a.chunked {
		// Flushing before the body commits the response to chunked encoding,
		// so the client sees Content-Length -1 and the cap has to be enforced
		// on the stream rather than on a declaration.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	_, _ = w.Write(a.body)
}

// installerServer wires an api.Server whose release source points at the fake.
func installerServer(t *testing.T, gh *fakeGitHub, rs ReleaseSource) (*Server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(gh)
	t.Cleanup(upstream.Close)
	if rs.Repo == "" {
		rs.Repo = testReleaseRepo
	}
	if rs.Version == "" {
		rs.Version = testReleaseVersion
	}
	rs.DownloadBase = upstream.URL
	rs.APIBase = upstream.URL
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Release:         rs,
	}), upstream
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func releaseWithInstaller() *fakeGitHub {
	gh := &fakeGitHub{}
	gh.add(fakeAsset{tag: testReleaseVersion, name: "install.sh", body: []byte("#!/usr/bin/env bash\necho installing\n")})
	gh.add(fakeAsset{tag: testReleaseVersion, name: "checksums.txt", body: []byte("abc  sigmad_0.3.0_linux_amd64.tar.gz\n")})
	gh.add(fakeAsset{tag: testReleaseVersion, name: "checksums.txt.sig", body: []byte("signature")})
	gh.add(fakeAsset{tag: testReleaseVersion, name: "checksums.txt.pem", body: []byte("-----BEGIN CERTIFICATE-----")})
	gh.add(fakeAsset{tag: testReleaseVersion, name: "sigmad.service", body: []byte("[Unit]\n")})
	gh.add(fakeAsset{tag: testReleaseVersion, name: "sigmad_0.3.0_linux_amd64.tar.gz", body: []byte("archive-amd64")})
	gh.add(fakeAsset{tag: testReleaseVersion, name: "sigmad_0.3.0_linux_arm64.tar.gz", body: []byte("archive-arm64")})
	return gh
}

func TestTheInstallScriptIsServedForTheVersionTheControlPlaneIsPinnedTo(t *testing.T) {
	gh := releaseWithInstaller()
	s, _ := installerServer(t, gh, ReleaseSource{})

	rec := get(t, s, "/install.sh")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /install.sh = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "echo installing") {
		t.Errorf("served body is not the release's install.sh: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Errorf("Content-Type = %q, want the shell-script type so a browser does not render the installer as a page", ct)
	}
}

func TestEveryAssetTheInstallerFetchesIsServedFromTheVersionedRoute(t *testing.T) {
	gh := releaseWithInstaller()
	s, _ := installerServer(t, gh, ReleaseSource{})

	for _, tc := range []struct{ asset, want string }{
		{"install.sh", "echo installing"},
		{"checksums.txt", "sigmad_0.3.0_linux_amd64.tar.gz"},
		{"checksums.txt.sig", "signature"},
		{"checksums.txt.pem", "BEGIN CERTIFICATE"},
		{"sigmad.service", "[Unit]"},
		{"sigmad_0.3.0_linux_amd64.tar.gz", "archive-amd64"},
		{"sigmad_0.3.0_linux_arm64.tar.gz", "archive-arm64"},
	} {
		t.Run(tc.asset, func(t *testing.T) {
			rec := get(t, s, "/dl/"+testReleaseVersion+"/"+tc.asset)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /dl/%s/%s = %d, want 200; body: %s", testReleaseVersion, tc.asset, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// The names install.sh actually fetches are read out of the script itself, so
// the allowlist cannot quietly stop covering one of them. Leaving sigmad.service
// off is the failure this exists for: it 404s nothing an operator can see,
// because install.sh falls back to an embedded unit — it just silently undoes
// SIGMA-155, which is there so a root ExecStart is checked against the
// cosign-signed checksums.txt.
func TestTheAllowlistCoversEveryAssetInstallShFetches(t *testing.T) {
	script := readInstallScriptForAllowlist(t)

	// The archive name is built from shell variables; expand it the way the
	// script does so the pattern half of the allowlist is covered too.
	archiveExpr := regexp.MustCompile(`(?m)^archive="([^"]+)"`).FindStringSubmatch(script)
	if archiveExpr == nil {
		t.Fatal(`install.sh has no top-level archive="..." assignment; if it was renamed, this test and allowedAsset must follow it`)
	}

	refs := regexp.MustCompile(`\$\{SIGMAHUB_DOWNLOAD_BASE\}/([^"'\s]+)`).FindAllStringSubmatch(script, -1)
	if len(refs) == 0 {
		t.Fatal("install.sh no longer fetches anything from ${SIGMAHUB_DOWNLOAD_BASE}; if the variable was renamed, the control plane's allowlist is now pinned to nothing")
	}

	for _, ref := range refs {
		for _, name := range expandAssetRef(ref[1], archiveExpr[1]) {
			if _, ok := allowedAsset(testReleaseVersion, name); !ok {
				t.Errorf("install.sh fetches %q from the control plane and the proxy's allowlist refuses it.\n"+
					"Add it to fixedAssets in installer.go — an asset the script fetches and the proxy will not serve either "+
					"fails the install outright or, for sigmad.service, degrades it silently to the unsigned fallback unit.", name)
			}
		}
	}
}

// expandAssetRef turns one ${SIGMAHUB_DOWNLOAD_BASE}/<ref> occurrence into the
// concrete asset names it can resolve to. Only ${archive} is a variable today,
// and it expands to one name per architecture the release publishes.
func expandAssetRef(ref, archiveExpr string) []string {
	if ref != "${archive}" {
		return []string{ref}
	}
	var out []string
	for _, arch := range store.SupportedArches() {
		name := strings.ReplaceAll(archiveExpr, "${ver_noV}", strings.TrimPrefix(testReleaseVersion, "v"))
		out = append(out, strings.ReplaceAll(name, "${arch}", arch))
	}
	return out
}

func readInstallScriptForAllowlist(t *testing.T) string {
	t.Helper()
	// Across the module boundary, the same way store/installer_vocabulary_test.go
	// reads it: cp cannot import agent, and this file is what both can read.
	b, err := os.ReadFile("../../../agent/packaging/install.sh")
	if err != nil {
		t.Fatalf("read agent/packaging/install.sh: %v", err)
	}
	return string(b)
}

func TestAnAssetOutsideTheAllowlistIsRefusedWithoutAskingGitHub(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asset string
	}{
		// A real asset of the same release — the control-plane archive is
		// published beside sigmad's and is still none of an onboarding host's
		// business.
		{"another product's archive", "sigmahub-cp_0.3.0_linux_amd64.tar.gz"},
		{"a file the release never published", ".env"},
		{"a plausible neighbour", "checksums.txt.bak"},
		{"an sbom", "sigmad_0.3.0_linux_amd64.tar.gz.sbom.json"},
		{"an escaped traversal out of the release", "..%2f..%2fchecksums.txt"},
		{"an escaped traversal to a dotfile", "..%2F..%2F.env"},
		{"a dot-dot prefix", "%2e%2e%2finstall.sh"},
		{"an absolute path", "%2fetc%2fpasswd"},
		{"an archive for a version this route did not name", "sigmad_9.9.9_linux_amd64.tar.gz"},
		{"an architecture the release does not publish", "sigmad_0.3.0_linux_riscv64.tar.gz"},
		{"a windows archive", "sigmad_0.3.0_windows_amd64.tar.gz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := releaseWithInstaller()
			s, _ := installerServer(t, gh, ReleaseSource{Token: testReleaseToken})

			rec := get(t, s, "/dl/"+testReleaseVersion+"/"+tc.asset)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET /dl/%s/%s = %d, want 404; body: %s", testReleaseVersion, tc.asset, rec.Code, rec.Body.String())
			}
			// The refusal must cost nothing upstream: an open proxy that asks
			// GitHub first is still an oracle for what a private repository
			// holds, and it spends the control plane's rate-limit budget on
			// whoever is probing it.
			if seen := gh.seen(); len(seen) != 0 {
				t.Errorf("a refused asset reached GitHub anyway: %+v", seen)
			}
		})
	}
}

func TestAVersionThatIsNotAReleaseTagIsRefusedWithoutAskingGitHub(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
	}{
		{"latest has no release assets", "latest"},
		{"an incomplete version", "v0.3"},
		{"an unprefixed version", "0.3.0"},
		{"a branch name", "main"},
		{"an escaped traversal", "..%2f..%2fsecret"},
		{"an escaped slash into another path", "v0.3.0%2f..%2fv9.9.9"},
		{"a query string smuggled into the segment", "v0.3.0%3Ffoo=bar"},
		{"an absolute url", "https:%2f%2fevil.example"},
		{"a dotfile", ".git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := releaseWithInstaller()
			s, _ := installerServer(t, gh, ReleaseSource{Token: testReleaseToken})

			rec := get(t, s, "/dl/"+tc.version+"/checksums.txt")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET /dl/%s/checksums.txt = %d, want 404; body: %s", tc.version, rec.Code, rec.Body.String())
			}
			if seen := gh.seen(); len(seen) != 0 {
				t.Errorf("a refused version reached GitHub anyway: %+v", seen)
			}
		})
	}
}

// The unescaped forms never reach the handler — net/http cleans the path and
// redirects — but the assertion that matters is the same one either way: no
// release bytes come back and GitHub is never asked.
func TestAnUnescapedTraversalNeverServesReleaseBytes(t *testing.T) {
	for _, path := range []string{
		"/dl/v0.3.0/../../etc/passwd",
		"/dl/../../../install.sh",
		"/dl/v0.3.0/./checksums.txt/../../../secret",
		"//dl/v0.3.0/checksums.txt",
	} {
		t.Run(path, func(t *testing.T) {
			gh := releaseWithInstaller()
			s, _ := installerServer(t, gh, ReleaseSource{Token: testReleaseToken})

			rec := get(t, s, path)
			if rec.Code == http.StatusOK {
				t.Fatalf("GET %s = 200 and served %q", path, rec.Body.String())
			}
			if seen := gh.seen(); len(seen) != 0 {
				t.Errorf("GET %s reached GitHub: %+v", path, seen)
			}
		})
	}
}

func TestTheReleaseTokenReachesGitHubAndNeverTheOperator(t *testing.T) {
	gh := releaseWithInstaller()
	s, _ := installerServer(t, gh, ReleaseSource{Token: testReleaseToken})

	rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checksums.txt = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	seen := gh.seen()
	if len(seen) != 2 {
		t.Fatalf("authenticated fetch made %d upstream requests, want 2 (release lookup, then asset download): %+v", len(seen), seen)
	}
	for _, req := range seen {
		if req.auth != "Bearer "+testReleaseToken {
			t.Errorf("upstream request %s carried Authorization %q, want the configured release credential", req.path, req.auth)
		}
	}
	if !strings.Contains(seen[0].path, "/releases/tags/"+testReleaseVersion) {
		t.Errorf("first upstream request was %q, want the release-by-tag lookup: the browser download URL does not accept a token on a private repository", seen[0].path)
	}
	if seen[1].accept != "application/octet-stream" {
		t.Errorf("asset download sent Accept %q; without application/octet-stream GitHub describes the asset instead of serving it", seen[1].accept)
	}

	// The whole response, headers included, must be free of the credential.
	dump := rec.Body.String()
	for k, v := range rec.Header() {
		dump += "\n" + k + ": " + strings.Join(v, ",")
	}
	if strings.Contains(dump, testReleaseToken) {
		t.Error("the release credential appeared in the response served to an unauthenticated caller")
	}
}

func TestAPublicReleaseIsProxiedWithNoCredentialConfigured(t *testing.T) {
	gh := releaseWithInstaller()
	s, _ := installerServer(t, gh, ReleaseSource{}) // no Token

	rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated fetch = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	seen := gh.seen()
	if len(seen) != 1 {
		t.Fatalf("unauthenticated fetch made %d upstream requests, want 1: %+v", len(seen), seen)
	}
	if seen[0].auth != "" {
		t.Errorf("unauthenticated fetch sent Authorization %q", seen[0].auth)
	}
	// The browser download URL, not the REST API. The API's anonymous limit is
	// 60 requests an hour and one onboarding costs six, so routing public
	// installs through it would break them at ten hosts an hour.
	if !strings.Contains(seen[0].path, "/releases/download/") {
		t.Errorf("unauthenticated fetch used %q, want the unmetered browser download URL", seen[0].path)
	}
}

func TestAnUpstream404NamesTheLikelyCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"public", ""},
		{"authenticated", testReleaseToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An empty release: the tag is well-formed and the asset is
			// allowlisted, so the refusal can only come from GitHub.
			s, _ := installerServer(t, &fakeGitHub{}, ReleaseSource{Token: tc.token})

			rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("upstream 404 answered %d, want it passed through as 404; body: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range []string{"checksums.txt", testReleaseVersion, "draft", "contents:read", "CP_RELEASE_TOKEN"} {
				if !strings.Contains(body, want) {
					t.Errorf("404 body does not mention %q, so it does not name the fix:\n%s", want, body)
				}
			}
			if strings.Contains(body, "documentation_url") {
				t.Errorf("GitHub's own error body was forwarded to the caller:\n%s", body)
			}
		})
	}
}

func TestARefusedCredentialIsNotReportedAsTheOperatorsForbidden(t *testing.T) {
	for _, upstream := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(upstream), func(t *testing.T) {
			gh := releaseWithInstaller()
			gh.forceStatus = upstream
			s, _ := installerServer(t, gh, ReleaseSource{Token: testReleaseToken})

			rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
			// Passing 403 straight through would tell the operator that THEY
			// are forbidden. They are not; the control plane is.
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("upstream %d answered %d, want 502: the caller is not the one being refused", upstream, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "CP_RELEASE_TOKEN") {
				t.Errorf("credential refusal does not name the setting that fixes it:\n%s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), testReleaseToken) {
				t.Error("the credential leaked into the error body")
			}
		})
	}
}

func TestAGitHubRateLimitIsReportedAsTemporaryAndNamesTheTokenAsTheFix(t *testing.T) {
	gh := releaseWithInstaller()
	gh.forceStatus = http.StatusTooManyRequests
	s, _ := installerServer(t, gh, ReleaseSource{})

	rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("upstream 429 answered %d, want 503 (retryable), body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "CP_RELEASE_TOKEN") {
		t.Errorf("rate-limit message does not mention that a token raises the limit:\n%s", rec.Body.String())
	}
}

func TestAnAssetLargerThanTheCapIsRefusedRatherThanStreamed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"public path checks Content-Length", ""},
		{"authenticated path checks the declared size", testReleaseToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeGitHub{}
			// Small enough that net/http declares a Content-Length for it, and
			// still far past the cap this proxy is given.
			gh.add(fakeAsset{tag: testReleaseVersion, name: "checksums.txt", body: []byte(strings.Repeat("A", 512))})
			s, _ := installerServer(t, gh, ReleaseSource{Token: tc.token, MaxBytes: 64})

			rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("oversized asset answered %d, want 502; body: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "AAAA") {
				t.Error("an oversized asset was streamed anyway; the declared size must be refused before a byte moves")
			}
		})
	}
}

// An upstream that declares nothing is the case the pre-flight size check
// cannot cover, so the copy itself has to stop. The host then fails checksum
// verification, which is the safe outcome — the unsafe one is buffering an
// unbounded body on an unauthenticated route.
func TestAStreamWithNoDeclaredLengthIsCutOffAtTheCap(t *testing.T) {
	gh := &fakeGitHub{}
	gh.add(fakeAsset{
		tag: testReleaseVersion, name: "checksums.txt",
		body: []byte(strings.Repeat("x", 4096)), chunked: true,
	})
	s, _ := installerServer(t, gh, ReleaseSource{MaxBytes: 64})

	rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
	if rec.Body.Len() != 64 {
		t.Fatalf("streamed %d bytes past a 64-byte cap; the cap must bound the copy, not only the declaration", rec.Body.Len())
	}
}

// Reaching the cap exactly is a complete asset, not a truncated one. Worth its
// own test because the obvious `n >= cap` spelling of the check calls it a
// truncation and tells the operator to go looking for a corrupted download that
// is not there.
func TestAnAssetOfExactlyTheCapIsServedWhole(t *testing.T) {
	gh := &fakeGitHub{}
	gh.add(fakeAsset{
		tag: testReleaseVersion, name: "checksums.txt",
		body: []byte(strings.Repeat("x", 64)), chunked: true,
	})
	s, _ := installerServer(t, gh, ReleaseSource{MaxBytes: 64})

	rec := get(t, s, "/dl/"+testReleaseVersion+"/checksums.txt")
	if rec.Code != http.StatusOK || rec.Body.Len() != 64 {
		t.Fatalf("an asset of exactly the cap answered %d with %d bytes, want 200 with 64", rec.Code, rec.Body.Len())
	}
}

func TestAnUnpinnedControlPlaneSaysHowToPinIt(t *testing.T) {
	for _, version := range []string{"", "dev", "latest", "v0.3"} {
		t.Run("version="+version, func(t *testing.T) {
			gh := releaseWithInstaller()
			s, _ := installerServer(t, gh, ReleaseSource{Version: "unset"})
			s.release.Version = version // "" cannot travel through installerServer's default

			rec := get(t, s, "/install.sh")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET /install.sh with version %q = %d, want 503; body: %s", version, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "CP_AGENT_VERSION") || !strings.Contains(body, "/dl/") {
				t.Errorf("the message names neither the setting nor the version-explicit URL:\n%s", body)
			}
			if seen := gh.seen(); len(seen) != 0 {
				t.Errorf("an unpinned control plane still asked GitHub: %+v", seen)
			}
		})
	}
}

func TestAControlPlaneWithNoReleaseRepositorySaysSoRatherThanGuessingOne(t *testing.T) {
	// newTestServer configures no Release at all, which is the shape every
	// handler unit test in this package builds.
	s := newTestServer(t, nil)
	for _, path := range []string{"/install.sh", "/dl/" + testReleaseVersion + "/checksums.txt"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503; body: %s", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "CP_RELEASE_REPO") {
			t.Errorf("GET %s does not name the setting that configures it:\n%s", path, rec.Body.String())
		}
	}
}

// Every failure body on these two routes ends up as stdin to `sudo bash` the
// moment an install command loses its -f, so each line has to be inert there.
func TestErrorBodiesAreInertWhenPipedIntoARootShell(t *testing.T) {
	gh := &fakeGitHub{}
	gh.forceStatus = http.StatusInternalServerError
	s, _ := installerServer(t, gh, ReleaseSource{Token: testReleaseToken})

	for _, path := range []string{
		"/install.sh",
		"/dl/" + testReleaseVersion + "/checksums.txt",
		"/dl/" + testReleaseVersion + "/not-an-asset",
		"/dl/not-a-tag/checksums.txt",
	} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, s, path)
			if rec.Code == http.StatusOK {
				t.Fatalf("GET %s unexpectedly succeeded", path)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("error Content-Type = %q, want text/plain", ct)
			}
			lines := strings.Split(strings.TrimRight(rec.Body.String(), "\n"), "\n")
			for _, line := range lines[:len(lines)-1] {
				if !strings.HasPrefix(line, "#") {
					t.Errorf("error body line %q is not a shell comment; piped into `sudo bash` it is a command run as root", line)
				}
			}
			if last := lines[len(lines)-1]; last != "exit 1" {
				t.Errorf("error body ends with %q, want `exit 1` so a pipe into bash fails instead of succeeding silently", last)
			}
		})
	}
}

// The allowlist is derived from the catalog's architecture list rather than
// retyped, so this asserts the derivation rather than the names: adding an
// architecture to the release makes its archive proxyable with no edit here.
func TestTheArchiveAllowlistFollowsTheArchitecturesTheReleasePublishes(t *testing.T) {
	for _, arch := range store.SupportedArches() {
		name := "sigmad_0.3.0_linux_" + arch + ".tar.gz"
		if _, ok := allowedAsset(testReleaseVersion, name); !ok {
			t.Errorf("%s is published by the release and the proxy refuses it", name)
		}
	}
	if _, ok := allowedAsset(testReleaseVersion, "sigmad_0.3.0_linux_s390x.tar.gz"); ok {
		t.Error("the proxy serves an architecture the release does not build, which is a free-form segment in the upstream URL")
	}
}
