package client

// SIGMA-323: the agent's 30-second mesh poll must be conditional.
//
// The control plane can only answer 304 if the agent asks conditionally, so the
// half of the fix that removes the O(N^2) traffic lives here: carry the ETag
// from the last answer into the next request, and treat a 304 as "reuse what
// you have" rather than as "you have no peers".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMeshPeersSendsValidatorAndHandlesNotModified(t *testing.T) {
	const etag = `"abc123"`
	var seenIfNoneMatch []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenIfNoneMatch = append(seenIfNoneMatch, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		ip := "10.8.0.1"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"self":  map[string]any{"serverId": "srv_1", "meshIp": &ip},
			"peers": []map[string]any{{"serverId": "srv_2", "pubkey": "pub2", "meshIp": "10.8.0.2"}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	res, gotETag, notModified, err := c.MeshPeers(context.Background(), "sat_test", "")
	if err != nil {
		t.Fatal(err)
	}
	if notModified {
		t.Fatal("an unconditional first fetch cannot be a 304")
	}
	if gotETag != etag {
		t.Fatalf("etag = %q, want %q", gotETag, etag)
	}
	if len(res.Peers) != 1 {
		t.Fatalf("peers = %d, want 1", len(res.Peers))
	}

	_, gotETag, notModified, err = c.MeshPeers(context.Background(), "sat_test", gotETag)
	if err != nil {
		t.Fatal(err)
	}
	if !notModified {
		t.Fatal("an unchanged peer set must come back as 304, not as the whole list again")
	}
	if gotETag != etag {
		t.Fatalf("the 304 lost the validator: %q", gotETag)
	}
	if len(seenIfNoneMatch) != 2 || seenIfNoneMatch[0] != "" || seenIfNoneMatch[1] != etag {
		t.Fatalf("If-None-Match sequence = %q, want [\"\", %q]", seenIfNoneMatch, etag)
	}
}

// A control plane that predates the validator returns 200 and no ETag. That
// must keep working — the agent simply goes on polling unconditionally.
func TestMeshPeersWithoutServerETagStaysUnconditional(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"self": map[string]any{"serverId": "srv_1"}, "peers": []any{}})
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, gotETag, notModified, err := c.MeshPeers(context.Background(), "sat_test", "")
	if err != nil || notModified || gotETag != "" {
		t.Fatalf("etag=%q notModified=%v err=%v", gotETag, notModified, err)
	}
}
