// Package reconciler renders per-server Desired-State Documents from resource
// specs and is the ONLY writer of DSDs. It is level-triggered: callers nudge
// it on a mutation, and a background loop resyncs the whole fleet every 60s so
// a missed nudge still converges.
package reconciler

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Store is the slice of the persistence layer the reconciler needs.
type Store interface {
	ResourceSpecsForServer(ctx context.Context, serverID string) ([]store.ResourceSpec, error)
	PendingDestructiveOpsForServer(ctx context.Context, orgID, serverID string) ([]store.PendingDestructiveOp, error)
	SecretRefsForServer(ctx context.Context, serverID string) (map[string][]store.SecretRefMeta, error)
	HostHardeningForServer(ctx context.Context, serverID string) (store.HostHardening, error)
	DomainsForServer(ctx context.Context, serverID string) (map[string][]store.Domain, error)
	StoreDSD(ctx context.Context, orgID, serverID string, ops []dsd.Op, docHash string, priv ed25519.PrivateKey) (dsd.Signed, bool, error)
	AllServerIDs(ctx context.Context) ([]struct{ ServerID, OrgID string }, error)
}

// Reconciler renders and versions DSDs and notifies long-poll waiters.
type Reconciler struct {
	log  *slog.Logger
	st   Store
	priv ed25519.PrivateKey
	acme ACMEConfig

	mu      sync.Mutex
	waiters map[string][]chan struct{} // serverID -> notify channels
}

func New(log *slog.Logger, st Store, priv ed25519.PrivateKey) *Reconciler {
	return &Reconciler{log: log, st: st, priv: priv, waiters: map[string][]chan struct{}{}}
}

// SetACMEConfig installs the ACME issuance config rendered into proxy.traefik
// ops (Let's Encrypt account email + CA directory; the staging/Pebble URL is
// injected for e2e). Called at boot before serving.
func (r *Reconciler) SetACMEConfig(cfg ACMEConfig) { r.acme = cfg }

// renderOps builds the ordered op list for a server. "app" resources fan into
// container ops (network.ensure → image.pull → volume.ensure → container.apply);
// other kinds keep the P1-2 no-op "resource.sync" stub until they are
// containerised. Confirmed destructive ops are appended as volume.remove.
func renderOps(serverID string, specs []store.ResourceSpec, pending []store.PendingDestructiveOp, secretRefs map[string][]store.SecretRefMeta, hardening store.HostHardening, domains map[string][]store.Domain, acme ACMEConfig) ([]dsd.Op, string) {
	networks := map[string]string{} // net op id -> network name (deduped per project)
	var resourceOps []dsd.Op

	for _, rs := range specs {
		if rs.Kind == "app" {
			if appOps, netID, ok := renderAppOps(rs, secretRefs[rs.ResourceID], domains[rs.ResourceID]); ok {
				resourceOps = append(resourceOps, appOps...)
				networks[netID] = dsd.NetworkName(rs.ProjectID)
				continue
			}
		}
		// Not yet containerised (or an app with no image): a no-op stub keeps the
		// resource represented in the DSD.
		stub, _ := json.Marshal(map[string]any{"resourceId": rs.ResourceID, "kind": rs.Kind, "spec": rs.Spec})
		resourceOps = append(resourceOps, dsd.Op{ID: "res:" + rs.ResourceID, Kind: dsd.KindResourceSync, Spec: stub})
	}

	// One network.ensure op per distinct project, emitted first in a stable
	// order so the rendered document (and thus its hash) is deterministic.
	netIDs := make([]string, 0, len(networks))
	for id := range networks {
		netIDs = append(netIDs, id)
	}
	sort.Strings(netIDs)
	ops := make([]dsd.Op, 0, len(netIDs)+len(resourceOps)+len(pending))
	for _, id := range netIDs {
		ns, _ := json.Marshal(map[string]string{"name": networks[id]})
		ops = append(ops, dsd.Op{ID: id, Kind: dsd.KindNetworkEnsure, Spec: ns})
	}
	ops = append(ops, resourceOps...)
	// Host-hardening ops (P1-5) are appended in a fixed order so the document
	// hash stays deterministic. They have no dependencies (host-level, independent
	// of the container graph).
	ops = append(ops, renderHostOps(serverID, hardening)...)
	// Ingress (P1-8): a proxy-role server runs Traefik. P1-5's nftables op has
	// already opened 80/443 (proxyRole feeds renderHostOps), so this only stands
	// up the proxy + ACME resolver; the router labels ride on the app containers.
	if hardening.ProxyRole {
		var serverDomains []store.Domain
		for _, ds := range domains {
			serverDomains = append(serverDomains, ds...)
		}
		ops = append(ops, renderTraefikOp(serverID, acme, serverDomains))
	}
	for _, p := range pending {
		ops = append(ops, renderVolumeRemoveOp(p))
	}
	return ops, dsd.SpecHash(ops)
}

// Reconcile renders a server's DSD; on a real change it bumps the version,
// signs, persists and wakes any long-poll waiter for that server.
func (r *Reconciler) Reconcile(ctx context.Context, orgID, serverID string) error {
	specs, err := r.st.ResourceSpecsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	pending, err := r.st.PendingDestructiveOpsForServer(ctx, orgID, serverID)
	if err != nil {
		return err
	}
	secretRefs, err := r.st.SecretRefsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	hardening, err := r.st.HostHardeningForServer(ctx, serverID)
	if err != nil {
		return err
	}
	domains, err := r.st.DomainsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	ops, hash := renderOps(serverID, specs, pending, secretRefs, hardening, domains, r.acme)
	_, changed, err := r.st.StoreDSD(ctx, orgID, serverID, ops, hash, r.priv)
	if err != nil {
		return err
	}
	if changed {
		r.log.Info("dsd rendered", "server", serverID, "ops", len(ops))
		r.notify(serverID)
	}
	return nil
}

// ReconcileAsync runs Reconcile in the background (fire-and-forget from an API
// handler that already returned) with its own short timeout.
func (r *Reconciler) ReconcileAsync(orgID, serverID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Reconcile(ctx, orgID, serverID); err != nil {
			r.log.Error("reconcile", "err", err, "server", serverID)
		}
	}()
}

// Run resyncs the whole fleet every interval until ctx is cancelled. Blocks;
// run in a goroutine.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			servers, err := r.st.AllServerIDs(ctx)
			if err != nil {
				r.log.Error("resync: list servers", "err", err)
				continue
			}
			for _, sv := range servers {
				if err := r.Reconcile(ctx, sv.OrgID, sv.ServerID); err != nil {
					r.log.Error("resync: reconcile", "err", err, "server", sv.ServerID)
				}
			}
		}
	}
}

// Wait returns a channel that closes when the server's DSD next changes, plus
// a cancel to release the subscription. Used by the long-poll handler.
func (r *Reconciler) Wait(serverID string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	r.mu.Lock()
	r.waiters[serverID] = append(r.waiters[serverID], ch)
	r.mu.Unlock()
	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		list := r.waiters[serverID]
		for i, c := range list {
			if c == ch {
				r.waiters[serverID] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
	return ch, cancel
}

func (r *Reconciler) notify(serverID string) {
	r.mu.Lock()
	list := r.waiters[serverID]
	delete(r.waiters, serverID)
	r.mu.Unlock()
	for _, ch := range list {
		close(ch)
	}
}
