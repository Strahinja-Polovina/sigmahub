package integration

import (
	"context"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestUpdateSecretValueInPlace pins SIGMA-264's store half: rotating a
// credential re-seals the value under the ACTIVE DEK while the row keeps its
// identity — same id, same name, same scope, same env/file mode — so every ref
// that names the secret still resolves and the change costs ONE config
// deployment instead of the two a delete-then-create pays for.
func TestUpdateSecretValueInPlace(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_secupd"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{
		ProjectID: proj.ID, EnvironmentID: env.ID, Name: "STRIPE_SECRET_KEY",
		Value: "sk_live_old", EnvVar: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := st.UpdateSecretValue(ctx, orgID, sec.ID, "sk_live_new", "admin")
	if err != nil {
		t.Fatalf("update secret value: %v", err)
	}
	if updated.ID != sec.ID {
		t.Fatalf("id = %q, want it unchanged (%q)", updated.ID, sec.ID)
	}
	if updated.Name != "STRIPE_SECRET_KEY" || !updated.EnvVar ||
		updated.EnvironmentID == nil || *updated.EnvironmentID != env.ID {
		t.Fatalf("metadata drifted on update: %+v", updated)
	}

	// The new value decrypts — the re-seal used the same AAD (org|project|id),
	// which is only true because the row identity did not move.
	val, err := st.RevealSecret(ctx, orgID, sec.ID, "admin")
	if err != nil || val != "sk_live_new" {
		t.Fatalf("reveal after update = %q err=%v, want sk_live_new", val, err)
	}

	// Exactly one row, still, and the mutation is audited like every other
	// secret write.
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE org_id=$1`, orgID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("secret rows = %d, want 1 (update must not create a second row)", n)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cp_audit_log WHERE org_id=$1 AND action='Secret value updated'`, orgID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows for the update = %d, want 1", n)
	}

	// An unknown id is a clean 404, not a silent no-op that would report a
	// rotation the operator never got.
	if _, err := st.UpdateSecretValue(ctx, orgID, "sec_nope", "x", "admin"); err == nil {
		t.Fatal("update of an unknown secret must fail")
	}
}
