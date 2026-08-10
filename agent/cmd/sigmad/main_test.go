package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/client"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/state"
)

// TestDSDLoopRetriesOn408 covers SIGMA-340: the DSD loop used to treat EVERY
// 4xx as terminal and return from the goroutine, with no way back short of a
// process restart. An operator fronting the control plane with nginx or
// Cloudflare can have a single 25-second long-poll come back 408 (or a burst
// earn a 429) — after which the host silently ignored every deploy, secret
// rotation and firewall change while the heartbeat kept the dashboard green.
func TestDSDLoopRetriesOn408(t *testing.T) {
	var polls atomic.Int32
	secondPoll := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/dsd" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch polls.Add(1) {
		case 1:
			// The proxy hiccup: a long-poll clipped by an upstream timeout rule.
			w.WriteHeader(http.StatusRequestTimeout)
		case 2:
			close(secondPoll)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	j, err := apply.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := state.State{ServerID: "srv_1", AgentToken: "tok", DSDPublicKey: "pk"}
	done := make(chan struct{})
	go func() {
		runDSDLoop(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), client.New(srv.URL), st, j, apply.NewRegistry(), nil)
		close(done)
	}()

	select {
	case <-secondPoll:
	case <-done:
		t.Fatal("DSD loop exited after a 408: the host has silently stopped applying")
	case <-time.After(15 * time.Second):
		t.Fatal("no second poll after a 408")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DSD loop did not stop on context cancel")
	}
}

// TestDSDLoopStopsOnRevokedCredential pins the other half: a 401/403 IS
// terminal — the agent's credential is dead and polling forever would only
// hammer the control plane.
func TestDSDLoopStopsOnRevokedCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	j, err := apply.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := state.State{ServerID: "srv_1", AgentToken: "tok", DSDPublicKey: "pk"}
	done := make(chan struct{})
	go func() {
		runDSDLoop(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), client.New(srv.URL), st, j, apply.NewRegistry(), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DSD loop kept polling with a revoked credential")
	}
}
