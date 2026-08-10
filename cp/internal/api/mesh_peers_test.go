package api

// SIGMA-323: mesh peer discovery must not restate a static answer to every
// agent every 30 seconds.
//
// Each agent calls GET /v1/agent/mesh/peers after every successful heartbeat,
// and the heartbeat interval is 30s. The handler answered with the org's whole
// peer list every time, with no version, no ETag and no change detection — so a
// 500-server org served 500 requests per 30 seconds, each one scanning and
// serialising ~499 rows. That is 250,000 rows and tens of megabytes of JSON
// every half minute to say nothing has changed, and it grows as the square of
// the org.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// meshStore serves a fixed peer set and counts how often the full list was
// actually read, so "answered without touching the peer list" is observable.
type meshStore struct {
	fakeStore
	peers     []store.MeshPeer
	peerReads int
}

func (m *meshStore) MeshPeers(context.Context, string, string) ([]store.MeshPeer, error) {
	m.peerReads++
	return m.peers, nil
}

// The real digest is an aggregate computed in Postgres over exactly the rows
// MeshPeers selects; this mirrors that contract without a database.
func (m *meshStore) MeshPeersDigest(context.Context, string, string) (string, error) {
	var sb strings.Builder
	for _, p := range m.peers {
		endpoint := ""
		if p.Endpoint != nil {
			endpoint = *p.Endpoint
		}
		fmt.Fprintf(&sb, "%s|%s|%s|%s|%s\n", p.ServerID, p.Name, p.Pubkey, p.MeshIP, endpoint)
	}
	return sb.String(), nil
}

func meshRequest(t *testing.T, s *Server, srv store.Server, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/mesh/peers", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	// Straight at the handler with the authenticated server in context: the
	// auth middleware is not what is under test here.
	req = req.WithContext(context.WithValue(req.Context(), serverCtxKey, srv))
	rec := httptest.NewRecorder()
	s.handleMeshPeers(rec, req)
	return rec
}

func TestMeshPeers_NotModifiedWhenPeerSetUnchanged(t *testing.T) {
	endpoint := "203.0.113.10:51820"
	st := &meshStore{peers: []store.MeshPeer{
		{ServerID: "srv_2", Name: "web-2", Pubkey: "pub2", MeshIP: "10.8.0.2", Endpoint: &endpoint},
		{ServerID: "srv_3", Name: "web-3", Pubkey: "pub3", MeshIP: "10.8.0.3"},
	}}
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), fakePinger{}, st, &fakeDomain{}, Options{})
	selfIP, selfPub := "10.8.0.1", "pub1"
	self := store.Server{ID: "srv_1", OrgID: "org_1", MeshIP: &selfIP, Pubkey: &selfPub}

	first := meshRequest(t, s, self, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first mesh peer fetch = %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the peer list carries no ETag, so an agent has no way to ask 'has it changed?'")
	}
	if !strings.Contains(first.Body.String(), "srv_2") {
		t.Fatalf("first response did not carry the peer list: %s", first.Body.String())
	}

	// Nothing has changed. The agent's next poll, 30 seconds later, must cost a
	// 304 and nothing else.
	second := meshRequest(t, s, self, etag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("second fetch of an unchanged peer set = %d, want 304 (the full list is being restated every 30s)", second.Code)
	}
	if body := second.Body.String(); body != "" {
		t.Fatalf("304 must have no body, got %q", body)
	}
	// RFC 7232: a 304 carries the validator, so the agent can keep polling
	// conditionally instead of falling back to unconditional fetches.
	if got := second.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}
	// The point of the 304 is the work it skips: an unchanged answer must not
	// scan, allocate and serialise the org's peer rows all over again.
	if st.peerReads != 1 {
		t.Fatalf("peer list read %d times across an unchanged 30s poll, want 1", st.peerReads)
	}

	// A membership change must invalidate it — a cache that cannot go stale is
	// only useful if it actually notices.
	st.peers = append(st.peers, store.MeshPeer{
		ServerID: "srv_4", Name: "web-4", Pubkey: "pub4", MeshIP: "10.8.0.4",
	})
	third := meshRequest(t, s, self, etag)
	if third.Code != http.StatusOK {
		t.Fatalf("fetch after a new peer joined = %d, want 200", third.Code)
	}
	if !strings.Contains(third.Body.String(), "srv_4") {
		t.Fatalf("the new peer was not served: %s", third.Body.String())
	}
	if third.Header().Get("ETag") == etag {
		t.Fatal("the ETag did not move when the peer set changed")
	}
}
