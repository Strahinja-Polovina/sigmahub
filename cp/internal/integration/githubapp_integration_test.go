package integration

// SIGMA-55 integration: linking a GitHub App installation to a connection and
// the clone-credential preference order — installation token first, stored
// PAT as the fallback, honest error when neither can produce a credential —
// plus the custody-wrapped import lifecycle of the App private key.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

type scriptedMinter struct {
	err   error
	calls int
}

func (m *scriptedMinter) InstallationToken(_ context.Context, installationID string) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return "ghs_installation:" + installationID, nil
}

func TestGitHubAppCloneCredentialPreference(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_ghapp"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "test"); err != nil {
		t.Fatal(err)
	}
	appSpec, _ := json.Marshal(map[string]any{"image": "nginx"})
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "acme/api", Token: "ghp_pat",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, ConnectionID: conn.ID,
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "abc123", ConfigHash: "cfg",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Baseline: no installation linked → the wrapped PAT.
	token, repo, provider, err := st.DeploymentCloneCredential(ctx, serverID, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token != "ghp_pat" || repo != "acme/api" || provider != "github" {
		t.Fatalf("PAT credential = %q %q %q", token, repo, provider)
	}

	// Linking validates the id and is org-scoped.
	if err := st.SetConnectionInstallation(ctx, orgID, conn.ID, "not-a-number", "test"); !errors.As(err, &store.ErrInvalid{}) {
		t.Fatalf("non-numeric installation id: err = %v, want ErrInvalid", err)
	}
	if err := st.SetConnectionInstallation(ctx, "org_other", conn.ID, "42", "test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-org link: err = %v, want ErrNotFound", err)
	}
	if err := st.SetConnectionInstallation(ctx, orgID, conn.ID, "42", "test"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetGitConnection(ctx, orgID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InstallationID != "42" {
		t.Fatalf("installationId = %q, want 42", got.InstallationID)
	}
	if n := auditCount(t, st, orgID, "test"); n == 0 {
		t.Fatal("linking an installation must audit")
	}

	// With a minter configured, the installation token wins.
	minter := &scriptedMinter{}
	st.SetInstallationTokens(minter)
	token, _, _, err = st.DeploymentCloneCredential(ctx, serverID, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token != "ghs_installation:42" {
		t.Fatalf("credential = %q, want minted installation token", token)
	}
	if minter.calls != 1 {
		t.Fatalf("minter called %d times, want 1", minter.calls)
	}

	// Minting failure falls back to the stored PAT — a GitHub App outage must
	// not break deploys for connections that still carry one.
	minter.err = fmt.Errorf("github is down")
	token, _, _, err = st.DeploymentCloneCredential(ctx, serverID, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token != "ghp_pat" {
		t.Fatalf("credential after mint failure = %q, want PAT fallback", token)
	}

	// App-only connection (no PAT): a mint failure is an error, not a silent
	// unauthenticated clone of a private repo.
	connApp, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "acme/apponly", InstallationID: "42",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	depApp, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, ConnectionID: connApp.ID,
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "def456", ConfigHash: "cfg2",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.DeploymentCloneCredential(ctx, serverID, depApp.ID); err == nil {
		t.Fatal("app-only connection with failed mint must error")
	}
	minter.err = nil
	token, _, _, err = st.DeploymentCloneCredential(ctx, serverID, depApp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token != "ghs_installation:42" {
		t.Fatalf("app-only credential = %q", token)
	}
}

func TestLoadGitHubAppKeyImportLifecycle(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	custody, err := kms.LoadOrCreateFileCustody(filepath.Join(t.TempDir(), "kms.key"), st.AuditUnwrapSink())
	if err != nil {
		t.Fatal(err)
	}

	// Unconfigured: no key, no error.
	key, err := st.LoadGitHubAppKey(ctx, custody, "")
	if err != nil || key != nil {
		t.Fatalf("unconfigured = (%v, %v), want (nil, nil)", key, err)
	}

	// Import the GitHub-downloaded PEM; it lands custody-wrapped in cp_secrets.
	gen, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(gen)})
	if err := os.WriteFile(pemPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err = st.LoadGitHubAppKey(ctx, custody, pemPath)
	if err != nil {
		t.Fatal(err)
	}
	if key == nil || key.N.Cmp(gen.N) != 0 {
		t.Fatal("imported key does not match the PEM")
	}

	// The file can now be deleted: later boots load the wrapped copy.
	if err := os.Remove(pemPath); err != nil {
		t.Fatal(err)
	}
	key, err = st.LoadGitHubAppKey(ctx, custody, "")
	if err != nil {
		t.Fatal(err)
	}
	if key == nil || key.N.Cmp(gen.N) != 0 {
		t.Fatal("stored key does not survive file removal")
	}

	// Rotation: a different PEM at the same path re-imports.
	gen2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(gen2)}), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err = st.LoadGitHubAppKey(ctx, custody, pemPath)
	if err != nil {
		t.Fatal(err)
	}
	if key == nil || key.N.Cmp(gen2.N) != 0 {
		t.Fatal("rotated key was not re-imported")
	}
	key, err = st.LoadGitHubAppKey(ctx, custody, "")
	if err != nil {
		t.Fatal(err)
	}
	if key == nil || key.N.Cmp(gen2.N) != 0 {
		t.Fatal("rotated key did not persist")
	}
}
