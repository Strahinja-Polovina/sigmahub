package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// verifyAppJWT checks the Authorization header carries a valid RS256 app JWT
// for the given key and returns its claims.
func verifyAppJWT(t *testing.T, authz string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	jwt := strings.TrimPrefix(authz, "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts, want 3", len(parts))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("jwt signature does not verify: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}

func TestInstallationTokenMintsAndCaches(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	cur := now // the clock the minter currently sees; advanced by the test
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		claims := verifyAppJWT(t, r.Header.Get("Authorization"), &key.PublicKey)
		if claims["iss"] != "12345" {
			t.Errorf("iss = %v, want 12345", claims["iss"])
		}
		exp, iat := int64(claims["exp"].(float64)), int64(claims["iat"].(float64))
		if iat >= cur.Unix() || exp <= cur.Unix() || exp-iat > 600 {
			t.Errorf("jwt window iat=%d exp=%d not sane around now=%d", iat, exp, cur.Unix())
		}
		calls++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_minted",
			"expires_at": cur.Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	a := NewAppAuth("12345", key)
	a.APIBase = srv.URL
	a.now = func() time.Time { return cur }

	tok, err := a.InstallationToken(context.Background(), "42")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "ghs_minted" {
		t.Fatalf("token = %q, want ghs_minted", tok)
	}

	// Second call inside the validity window is served from the cache.
	if _, err := a.InstallationToken(context.Background(), "42"); err != nil {
		t.Fatalf("cached mint: %v", err)
	}
	if calls != 1 {
		t.Fatalf("GitHub called %d times, want 1 (cache)", calls)
	}

	// Advance into the expiry margin: the next call must re-mint.
	cur = now.Add(56 * time.Minute)
	if _, err := a.InstallationToken(context.Background(), "42"); err != nil {
		t.Fatalf("refresh mint: %v", err)
	}
	if calls != 2 {
		t.Fatalf("GitHub called %d times after expiry, want 2", calls)
	}
}

func TestInstallationTokenErrors(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"integration not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	a := NewAppAuth("12345", key)
	a.APIBase = srv.URL

	if _, err := a.InstallationToken(context.Background(), ""); err == nil {
		t.Fatal("empty installation id must error")
	}
	if _, err := a.InstallationToken(context.Background(), "42"); err == nil {
		t.Fatal("a 404 from GitHub must surface as an error, not an empty token")
	}
}
