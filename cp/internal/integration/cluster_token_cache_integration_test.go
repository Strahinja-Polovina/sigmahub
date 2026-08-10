package integration

// SIGMA-319: the cluster join token must be custody-unwrapped once per process,
// not once per reconcile.
//
// Every Reconcile calls ClusterMembershipForServer, and that used to unwrap the
// wrapped join token unconditionally. With Vault custody an unwrap is an HTTP
// round trip to transit AND an audit row, so a 50-node cluster on the 60s
// resync produced 72,000 Vault decrypts and 72,000 "Key unwrapped" rows a day
// for a token that never changes.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// countingCustody delegates to a real custody and counts unwraps per purpose.
type countingCustody struct {
	kms.KeyCustody
	mu     sync.Mutex
	counts map[string]int
}

func (c *countingCustody) Unwrap(ctx context.Context, purpose string, envelope []byte) ([]byte, error) {
	c.mu.Lock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[purpose]++
	c.mu.Unlock()
	return c.KeyCustody.Unwrap(ctx, purpose, envelope)
}

func (c *countingCustody) count(purpose string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[purpose]
}

func TestClusterJoinTokenUnwrappedOncePerProcess(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	orgID := "org_clustertok"

	// Swap in a counting custody BEFORE the cluster is created, so the token is
	// wrapped and unwrapped through the same instance.
	base, err := kms.LoadOrCreateFileCustody(t.TempDir()+"/kms.key", st.AuditUnwrapSink())
	if err != nil {
		t.Fatal(err)
	}
	custody := &countingCustody{KeyCustody: base}
	st.SetCustody(custody)

	serverID := connectServer(t, st, orgID, "cp-node")
	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "admin"); err != nil {
		t.Fatal(err)
	}
	cls, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: env.ID, Name: "prod", ControlPlaneID: serverID,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	purpose := "cluster_token:" + orgID
	rec := reconciler.New(slog.New(slog.NewTextHandler(io.Discard, nil)), st, key)

	// The first reconcile has to unwrap: nothing is memoised yet, and the node
	// op carries the token.
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if got := custody.count(purpose); got != 1 {
		t.Fatalf("first reconcile unwrapped %d times, want 1", got)
	}
	// The token has not changed, so the resync's re-render must not go back to
	// custody. This is the assertion the ticket is about: before the memo it
	// was 2, and it grew by one for every 60s pass, forever.
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if got := custody.count(purpose); got != 1 {
		t.Fatalf("second reconcile unwrapped again: total %d, want 1", got)
	}

	// The render must still carry the real token — a memo that returns the
	// wrong value (or none) would silently stop workers joining.
	m, ok, err := st.ClusterMembershipForServer(ctx, serverID)
	if err != nil || !ok {
		t.Fatalf("membership: ok=%v err=%v", ok, err)
	}
	if m.JoinToken == "" || m.ClusterID != cls.ID {
		t.Fatalf("memoised membership lost its token: %+v", m)
	}
	if got := custody.count(purpose); got != 1 {
		t.Fatalf("membership read after the memo unwrapped again: total %d, want 1", got)
	}

	// A token that DOES change must invalidate the memo — otherwise the memo
	// would be a correctness bug the moment a cluster is recreated in the same
	// process (same server, new cluster row, new token).
	rotated, err := custody.Wrap(ctx, purpose, []byte("rotated-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE clusters SET join_token_wrapped = $2 WHERE id = $1`, cls.ID, rotated); err != nil {
		t.Fatal(err)
	}
	m2, ok, err := st.ClusterMembershipForServer(ctx, serverID)
	if err != nil || !ok {
		t.Fatalf("membership after rotation: ok=%v err=%v", ok, err)
	}
	if m2.JoinToken != "rotated-token" {
		t.Fatalf("stale token served after rotation: %q", m2.JoinToken)
	}
	if got := custody.count(purpose); got != 2 {
		t.Fatalf("rotation must cost exactly one more unwrap: total %d, want 2", got)
	}
}
