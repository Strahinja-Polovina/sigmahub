package deploy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubCP is enough control plane to run smoke.sh against: it mints org-scoped
// Org Admin tokens, enforces which org each one may read, and — the point of
// this file — remembers which of them were revoked.
//
// A stub rather than the real server because what is under test is the SCRIPT's
// custody of the credentials it is handed, which is the same question whichever
// process minted them, and because the real one needs Postgres.
type stubCP struct {
	mu sync.Mutex
	// token → org it is Org Admin on. Deleted on revoke, so a revoked token
	// 401s exactly as it would against the real control plane.
	tokens  map[string]string
	revoked []string
	n       int
}

const stubProvisionToken = "prov-token-for-the-smoke-test"

func newStubCP() *stubCP { return &stubCP{tokens: map[string]string{}} }

func (c *stubCP) bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// orgOf authorizes a service-token call: 401 when the token is unknown, 403
// when it belongs to a different org (the cross-tenant assertion smoke.sh
// makes in step 7).
func (c *stubCP) authorize(w http.ResponseWriter, r *http.Request, org string) bool {
	c.mu.Lock()
	owner, known := c.tokens[c.bearer(r)]
	c.mu.Unlock()
	if !known {
		writeStub(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	if owner != org {
		writeStub(w, http.StatusForbidden, map[string]string{"error": "cross-tenant"})
		return false
	}
	return true
}

func writeStub(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (c *stubCP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeStub(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		writeStub(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /v1/orgs", func(w http.ResponseWriter, r *http.Request) {
		if c.bearer(r) != stubProvisionToken {
			writeStub(w, http.StatusUnauthorized, map[string]string{"error": "provision token required"})
			return
		}
		var req struct {
			OrgID string `json:"orgId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.mu.Lock()
		c.n++
		tok := fmt.Sprintf("svc_secret_%d", c.n)
		id := fmt.Sprintf("tok_%d", c.n)
		c.tokens[tok] = req.OrgID
		c.mu.Unlock()
		writeStub(w, http.StatusCreated, map[string]string{
			"orgId": req.OrgID, "token": tok, "tokenId": id, "role": "org_admin",
		})
	})
	mux.HandleFunc("GET /v1/orgs/{orgId}/projects", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorize(w, r, r.PathValue("orgId")) {
			return
		}
		writeStub(w, http.StatusOK, map[string]any{"projects": []any{}})
	})
	mux.HandleFunc("POST /v1/orgs/{orgId}/projects", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorize(w, r, r.PathValue("orgId")) {
			return
		}
		writeStub(w, http.StatusCreated, map[string]string{"id": "prj_smoke"})
	})
	mux.HandleFunc("POST /v1/orgs/{orgId}/projects/{projectId}/environments", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorize(w, r, r.PathValue("orgId")) {
			return
		}
		writeStub(w, http.StatusCreated, map[string]string{"id": "env_smoke"})
	})
	mux.HandleFunc("GET /v1/orgs/{orgId}/projects/{projectId}/environments", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorize(w, r, r.PathValue("orgId")) {
			return
		}
		writeStub(w, http.StatusOK, map[string]any{"environments": []any{}})
	})
	mux.HandleFunc("POST /v1/orgs/{orgId}/bootstrap-tokens", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorize(w, r, r.PathValue("orgId")) {
			return
		}
		writeStub(w, http.StatusCreated, map[string]string{"token": "boot_secret"})
	})
	mux.HandleFunc("GET /v1/orgs/{orgId}/servers", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorize(w, r, r.PathValue("orgId")) {
			return
		}
		writeStub(w, http.StatusOK, map[string]any{"servers": []any{}})
	})
	mux.HandleFunc("DELETE /v1/orgs/{orgId}/service-tokens/{tokenId}", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorize(w, r, r.PathValue("orgId")) {
			return
		}
		c.mu.Lock()
		c.revoked = append(c.revoked, r.PathValue("tokenId"))
		delete(c.tokens, c.bearer(r))
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// smoke.sh must not leave the Org Admin credentials it mints readable on disk,
// and must not leave them usable (SIGMA-267).
//
// The script wrote every response body — including the provision response, which
// carries a freshly minted Org Admin token in plaintext — to the fixed path
// /tmp/smoke.body, with the process umask, and never removed it. It also left
// two live Org Admin tokens behind on every run. Both matter more now than they
// did when smoke.sh was a thing you ran by hand: staging.md tells the operator
// to run it after every bring-up, and the deploy workflow now runs it on every
// push to main (SIGMA-265), on a box that by design also runs design-partner
// workloads. Any local process could read the file and hold org-admin authority
// indefinitely.
func TestSmokeLeavesNoCredentialsOnDisk(t *testing.T) {
	cp := newStubCP()
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	// TMPDIR is where a well-behaved script puts its scratch files, so point it
	// somewhere this test owns: anything the script leaves lands here and is
	// visible to the assertions below. The pre-fix script ignored it and wrote
	// to a hard-coded /tmp path, which is checked separately.
	tmp := t.TempDir()
	// Whether the hard-coded scratch path is touched at all. Sampled rather
	// than deleted: it lives in a directory this test does not own. The window
	// in which it holds the Org Admin token is the whole rest of the run, so
	// "what does it contain when the script exits" is the wrong question — the
	// question is whether a predictable, non-0600 path is written at all.
	beforeFixedPath, hadFixedPath := os.Stat(fixedSmokeBodyPath)

	cmd := exec.Command("bash", "smoke.sh")
	cmd.Env = append(os.Environ(),
		"CP_URL="+srv.URL,
		"CP_PROVISION_TOKEN="+stubProvisionToken,
		"SMOKE_ORG=smoke-credential-custody",
		"TMPDIR="+tmp,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke.sh failed against the stub control plane: %v\n%s", err, out)
	}

	cp.mu.Lock()
	minted := make([]string, 0, len(cp.tokens))
	for tok := range cp.tokens {
		minted = append(minted, tok)
	}
	revoked := append([]string(nil), cp.revoked...)
	cp.mu.Unlock()

	// Every token the run minted must have been revoked — which, because the
	// stub forgets a revoked token exactly as the control plane does, is the
	// same statement as "the token 401s afterwards": nothing is left in
	// cp.tokens for a later caller to authenticate with.
	if len(minted) != 0 {
		t.Errorf("smoke.sh left %d live Org Admin token(s) behind: %v", len(minted), minted)
	}
	if len(revoked) != 2 {
		t.Errorf("expected both minted Org Admin tokens to be revoked, got %v", revoked)
	}

	// Nothing under TMPDIR may still hold a credential. The token values are
	// not known here (they are the stub's), so scan for the prefix every stub
	// token shares plus the bootstrap token's.
	for _, secret := range []string{"svc_secret_", "boot_secret"} {
		if path := fileContaining(t, tmp, secret); path != "" {
			t.Errorf("smoke.sh left %q on disk at %s", secret, path)
		}
	}

	// And the hard-coded path specifically. Every response body went here,
	// starting with the provision response that carries the Org Admin token in
	// plaintext, at a path any local process can guess, with the process umask
	// rather than 0600, and it was never removed.
	afterFixedPath, hasFixedPath := os.Stat(fixedSmokeBodyPath)
	switch {
	case hasFixedPath == nil && hadFixedPath != nil:
		t.Errorf("smoke.sh created the predictable, world-guessable path %s", fixedSmokeBodyPath)
	case hasFixedPath == nil && hadFixedPath == nil &&
		afterFixedPath.ModTime().After(beforeFixedPath.ModTime()):
		t.Errorf("smoke.sh wrote to the predictable, world-guessable path %s", fixedSmokeBodyPath)
	}
}

// The path smoke.sh used to write every response body to, including the one
// carrying a freshly minted Org Admin token.
const fixedSmokeBodyPath = "/tmp/smoke.body"

// fileContaining returns the first file under dir whose contents include needle,
// or "" when there is none.
func fileContaining(t *testing.T, dir, needle string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil //nolint:nilerr // an unreadable entry is not this test's subject
		}
		b, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(b), needle) {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}
