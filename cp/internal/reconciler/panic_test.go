package reconciler

// SIGMA-250: a panic in the fleet resync used to take the whole control plane
// with it. The resync walks every server in ONE goroutine, so one org's
// resource spec driving a nil dereference or an out-of-range index in a render
// helper killed the process for every tenant — and, because the resync is
// level-triggered and runs every 60 seconds, it killed it again 60 seconds
// after each restart, until somebody found and hand-edited the offending row.

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// panicStore reconciles every server normally except one, whose spec read
// panics — standing in for a render helper that trips over an unexpected shape.
type panicStore struct {
	mu        sync.Mutex
	panicOn   string
	reconcled map[string]int
	servers   []struct{ ServerID, OrgID string }
}

func (p *panicStore) seen(serverID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reconcled == nil {
		p.reconcled = map[string]int{}
	}
	p.reconcled[serverID]++
}

func (p *panicStore) count(serverID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reconcled[serverID]
}

func (p *panicStore) ResourceSpecsForServer(_ context.Context, serverID string) ([]store.ResourceSpec, error) {
	p.seen(serverID)
	if serverID == p.panicOn {
		var spec *store.ResourceSpec
		// A nil dereference, exactly as a render helper would produce it.
		return []store.ResourceSpec{*spec}, nil
	}
	return nil, nil
}

func (p *panicStore) ClusterMembershipForServer(context.Context, string) (store.ClusterMembership, bool, error) {
	return store.ClusterMembership{}, false, nil
}
func (p *panicStore) ResourceSpecsForCluster(context.Context, string) ([]store.ResourceSpec, error) {
	return nil, nil
}
func (p *panicStore) ClusterBuildSpecsForServer(context.Context, string) ([]store.ResourceSpec, error) {
	return nil, nil
}
func (p *panicStore) DeployTargetForResource(context.Context, string) (store.DeployTarget, error) {
	return store.DeployTarget{}, nil
}
func (p *panicStore) ImageRepositoryForOrg(context.Context, string) (string, error) { return "", nil }
func (p *panicStore) PendingDestructiveOpsForServer(context.Context, string, string) ([]store.PendingDestructiveOp, error) {
	return nil, nil
}
func (p *panicStore) SecretRefsForServer(context.Context, string) (map[string][]store.SecretRefMeta, error) {
	return nil, nil
}
func (p *panicStore) HostHardeningForServer(context.Context, string) (store.HostHardening, error) {
	return store.HostHardening{}, nil
}
func (p *panicStore) DomainsForServer(context.Context, string) (map[string][]store.Domain, error) {
	return nil, nil
}
func (p *panicStore) DeployTargetsForServer(context.Context, string) (map[string]store.DeployTarget, error) {
	return map[string]store.DeployTarget{}, nil
}
func (p *panicStore) DBTargetsForServer(context.Context, string) (map[string]store.DBTarget, error) {
	return nil, nil
}
func (p *panicStore) S3TargetsForServer(context.Context, string) (map[string]store.S3Target, error) {
	return nil, nil
}
func (p *panicStore) LLMTargetsForServer(context.Context, string) (map[string]store.LLMTarget, error) {
	return nil, nil
}
func (p *panicStore) PendingS3OpsForServer(context.Context, string) ([]store.S3OpSpec, error) {
	return nil, nil
}
func (p *panicStore) BackupRunsForServer(context.Context, string) ([]store.BackupRunSpec, error) {
	return nil, nil
}
func (p *panicStore) StoreDSD(_ context.Context, orgID, serverID string, ops []dsd.Op, _ string, _ ed25519.PrivateKey) (dsd.Signed, bool, error) {
	return dsd.Signed{Document: dsd.Document{Version: 1, OrgID: orgID, ServerID: serverID, Ops: ops}}, false, nil
}
func (p *panicStore) StampDeploymentDSDVersion(context.Context, []string, int64) error { return nil }
func (p *panicStore) AllServerIDs(context.Context) ([]struct{ ServerID, OrgID string }, error) {
	return p.servers, nil
}
func (p *panicStore) LockServerReconcile(context.Context, string) (func(), bool, error) {
	return func() {}, true, nil
}

var _ Store = (*panicStore)(nil)

func TestResyncSurvivesPanickingServer(t *testing.T) {
	st := &panicStore{
		panicOn: "srv_b",
		servers: []struct{ ServerID, OrgID string }{
			{"srv_a", "org_1"}, {"srv_b", "org_2"}, {"srv_c", "org_3"},
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := New(log, st, nil)

	var resyncErrs int
	var mu sync.Mutex
	rec.SetObservers(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			resyncErrs++
		}
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx, 10*time.Millisecond)

	// srv_c is reconciled AFTER the panicking srv_b in AllServerIDs order, so
	// seeing it twice proves both that the panic was contained and that the loop
	// kept ticking rather than dying on the next pass.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st.count("srv_c") >= 2 && st.count("srv_a") >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	if got := st.count("srv_a"); got < 2 {
		t.Fatalf("srv_a reconciled %d times, want >= 2", got)
	}
	if got := st.count("srv_c"); got < 2 {
		t.Fatalf("srv_c reconciled %d times, want >= 2 (the panicking server ahead of it stopped the pass)", got)
	}
	// The pass is reported as FAILED, so a quarantined server is not silently
	// skipped — the resync's last-success clock goes stale and something alerts.
	mu.Lock()
	defer mu.Unlock()
	if resyncErrs == 0 {
		t.Fatal("a resync pass that panicked on a server reported success")
	}
}
