package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AppAuth mints GitHub App installation access tokens (SIGMA-55): a
// short-lived RS256 app JWT — signed with the App private key that is held
// KMS-custody-wrapped at rest and unwrapped into process memory at boot —
// exchanged at POST /app/installations/{id}/access_tokens for a ~1h
// installation token. Tokens are cached per installation and refreshed with a
// safety margin, so the deploy clone path and the repo inspector never see a
// token that is about to expire mid-operation.
type AppAuth struct {
	appID   string
	key     *rsa.PrivateKey
	Client  *http.Client
	APIBase string
	// now is injectable for expiry tests.
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedInstallationToken
}

type cachedInstallationToken struct {
	token     string
	expiresAt time.Time
}

// tokenExpiryMargin refreshes a cached token this long before GitHub's
// expires_at: an op that starts near the boundary must not clone with a token
// that dies mid-fetch.
const tokenExpiryMargin = 5 * time.Minute

// NewAppAuth returns a minter for the given App id and private key.
func NewAppAuth(appID string, key *rsa.PrivateKey) *AppAuth {
	return &AppAuth{
		appID:   appID,
		key:     key,
		Client:  &http.Client{Timeout: 10 * time.Second},
		APIBase: DefaultAPIBase,
		now:     time.Now,
		cache:   map[string]cachedInstallationToken{},
	}
}

// appJWT builds the RS256-signed App JWT GitHub requires on /app/* calls.
// iat is backdated 60s against clock drift; exp is capped at GitHub's 10min
// maximum (9min here for the same drift headroom).
func (a *AppAuth) appJWT(now time.Time) (string, error) {
	b64 := func(v any) (string, error) {
		j, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(j), nil
	}
	header, err := b64(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := b64(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// InstallationToken returns a valid installation access token, minting a
// fresh one when the cache misses or the cached token is inside the expiry
// margin. Safe for concurrent use.
func (a *AppAuth) InstallationToken(ctx context.Context, installationID string) (string, error) {
	installationID = strings.TrimSpace(installationID)
	if installationID == "" {
		return "", fmt.Errorf("installation id is empty")
	}
	now := a.now()

	a.mu.Lock()
	if c, ok := a.cache[installationID]; ok && now.Add(tokenExpiryMargin).Before(c.expiresAt) {
		a.mu.Unlock()
		return c.token, nil
	}
	a.mu.Unlock()

	jwt, err := a.appJWT(now)
	if err != nil {
		return "", err
	}
	base := a.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", base, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := a.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github installation token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github installation token for %s: unexpected status %s", installationID, resp.Status)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("github installation token: decode: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("github installation token: empty token in response")
	}

	a.mu.Lock()
	a.cache[installationID] = cachedInstallationToken{token: out.Token, expiresAt: out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// InstallationAccount is the GitHub account an App installation belongs to.
type InstallationAccount struct {
	Login string `json:"login"`
	Type  string `json:"type"` // User|Organization
}

// Account reads the account an installation is installed on, so the dashboard
// can name the integration ("Connected to acme-corp") rather than showing a
// bare numeric id. Uses the App JWT (not an installation token), which is why
// it lives on AppAuth.
func (a *AppAuth) Account(ctx context.Context, installationID string) (InstallationAccount, error) {
	if !isNumeric(installationID) {
		return InstallationAccount{}, fmt.Errorf("installation id must be numeric")
	}
	jwt, err := a.appJWT(a.nowOrDefault())
	if err != nil {
		return InstallationAccount{}, err
	}
	base := a.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/app/installations/"+installationID, nil)
	if err != nil {
		return InstallationAccount{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+jwt)

	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return InstallationAccount{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return InstallationAccount{}, fmt.Errorf("read installation: %s: %s", resp.Status, string(snippet))
	}
	var payload struct {
		Account InstallationAccount `json:"account"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return InstallationAccount{}, fmt.Errorf("decode installation: %w", err)
	}
	return payload.Account, nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// nowOrDefault is the injectable clock, defaulted.
func (a *AppAuth) nowOrDefault() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}
