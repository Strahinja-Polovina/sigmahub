package integration

// SIGMA-323: the mesh peer-set fingerprint has to track the peer set exactly.
//
// The digest is what lets GET /v1/agent/mesh/peers answer 304 to the poll every
// agent makes every 30 seconds. That makes it load-bearing in a way a cache key
// usually is not: a digest that fails to move when a server joins does not make
// the product slow, it makes the new host unreachable on the mesh forever,
// because no agent ever re-fetches the list. This test drives it against the
// real schema and the real query.

import (
	"context"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestMeshPeersDigestTracksThePeerSet(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_meshdigest"

	self := connectServer(t, st, orgID, "self")
	peer := connectServer(t, st, orgID, "peer")
	// A peer is only a peer once it has both a pubkey and a mesh IP; the mesh IP
	// is allocated at registration, the pubkey arrives on a heartbeat.
	if err := st.RecordHeartbeat(ctx, peer, store.HeartbeatInput{Pubkey: "pub-peer"}); err != nil {
		t.Fatal(err)
	}

	first, err := st.MeshPeersDigest(ctx, orgID, self)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("empty digest: the query returned nothing to key a 304 on")
	}
	// Stable across calls when nothing changed — otherwise every poll is a miss
	// and the validator buys nothing at all.
	again, err := st.MeshPeersDigest(ctx, orgID, self)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("digest moved with no change: %q then %q", first, again)
	}

	// A new peer must move it.
	joiner := connectServer(t, st, orgID, "joiner")
	if err := st.RecordHeartbeat(ctx, joiner, store.HeartbeatInput{Pubkey: "pub-joiner"}); err != nil {
		t.Fatal(err)
	}
	afterJoin, err := st.MeshPeersDigest(ctx, orgID, self)
	if err != nil {
		t.Fatal(err)
	}
	if afterJoin == first {
		t.Fatal("a server joined the mesh and the digest did not move — every agent would keep getting 304 and never learn about it")
	}

	// So must a changed endpoint, which is what NAT re-probing updates.
	if err := st.RecordHeartbeat(ctx, peer, store.HeartbeatInput{
		Pubkey: "pub-peer", Endpoint: "203.0.113.7:51820",
	}); err != nil {
		t.Fatal(err)
	}
	afterEndpoint, err := st.MeshPeersDigest(ctx, orgID, self)
	if err != nil {
		t.Fatal(err)
	}
	if afterEndpoint == afterJoin {
		t.Fatal("a peer's endpoint changed and the digest did not move — tunnels would keep dialling the old address")
	}

	// The digest is per-requester: it excludes the asking server, so two agents
	// in the same org legitimately hold different validators.
	fromPeer, err := st.MeshPeersDigest(ctx, orgID, peer)
	if err != nil {
		t.Fatal(err)
	}
	if fromPeer == afterEndpoint {
		t.Fatal("the digest does not exclude the requesting server")
	}

	// And it agrees with what MeshPeers actually serves.
	peers, err := st.MeshPeers(ctx, orgID, self)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("MeshPeers returned %d peers, want 2", len(peers))
	}
}
