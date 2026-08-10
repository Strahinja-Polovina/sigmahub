package selfupdate

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// recordingTransport answers every request from an in-memory asset table and
// remembers the URL. It replaces the network entirely so the test can assert
// on WHERE the updater decided to fetch from, which is the whole question here.
type recordingTransport struct {
	mu   sync.Mutex
	urls []string
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.urls = append(t.urls, req.URL.String())
	t.mu.Unlock()
	// Any body will do: the update fails at cosign verification long before the
	// bytes matter, and the assertions are on the request URLs.
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader("placeholder\n")),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func (t *recordingTransport) seen() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.urls))
	copy(out, t.urls)
	return out
}

func runUpdate(t *testing.T, spec string) *recordingTransport {
	t.Helper()
	if runtime.GOOS != "linux" || !ArchSupported(runtime.GOARCH) {
		t.Skipf("self-update is linux/%s only", strings.Join(supportedArches, "|"))
	}
	tr := &recordingTransport{}
	u := &Updater{
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		CurrentVersion: "v0.1.0",
		HTTP:           &http.Client{Transport: tr},
	}
	// The update is EXPECTED to fail: the placeholder bytes are not a signed
	// release. It fails after the four downloads have been attempted, which is
	// all this test needs.
	if err := u.handle(context.Background(), dsd.Op{
		ID: "agent:update:srv_1:v0.2.0", Kind: Kind, Spec: json.RawMessage(spec),
	}); err == nil {
		t.Fatal("expected the placeholder release to fail verification")
	}
	return tr
}

// TestSelfUpdateUsesDownloadBaseFromSpec is SIGMA-262: when the control plane
// tells the agent where to download the release from, the agent must use it.
//
// An operator running against a PRIVATE release repository onboards every host
// through the control plane's /dl proxy (SIGMA-217), because github.com 404s
// them. If self-update then reaches for github.com directly, every dashboard
// upgrade fails with a 404 and the only remaining upgrade path is SSH — the
// exact thing the upgrade button exists to remove.
func TestSelfUpdateUsesDownloadBaseFromSpec(t *testing.T) {
	const base = "https://cp.example.test/dl/v0.2.0"
	tr := runUpdate(t, `{"version":"v0.2.0","downloadBase":"`+base+`"}`)

	urls := tr.seen()
	if len(urls) != 4 {
		t.Fatalf("expected 4 asset downloads, got %d: %v", len(urls), urls)
	}
	want := map[string]bool{
		ArchiveName("v0.2.0", runtime.GOARCH): false,
		"checksums.txt":                       false,
		"checksums.txt.sig":                   false,
		"checksums.txt.pem":                   false,
	}
	for _, u := range urls {
		if strings.Contains(u, "github.com") {
			t.Errorf("downloaded %s from github.com even though the control plane supplied %s", u, base)
			continue
		}
		if !strings.HasPrefix(u, base+"/") {
			t.Errorf("download %s is not under the supplied base %s", u, base)
			continue
		}
		name := path.Base(u)
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected asset %q", name)
			continue
		}
		want[name] = true
	}
	for name, got := range want {
		if !got {
			t.Errorf("asset %q was not fetched from the supplied base", name)
		}
	}
}

// TestSelfUpdateFallsBackToGitHubWithoutDownloadBase pins the other half of the
// contract: a control plane that predates SIGMA-262 sends no downloadBase, and
// its fleet must keep upgrading from the public release repo.
func TestSelfUpdateFallsBackToGitHubWithoutDownloadBase(t *testing.T) {
	tr := runUpdate(t, `{"version":"v0.2.0"}`)
	for _, u := range tr.seen() {
		if !strings.HasPrefix(u, "https://github.com/"+DefaultRepo+"/releases/download/v0.2.0/") {
			t.Errorf("without a download base the agent must use the release repo; got %s", u)
		}
	}
}
