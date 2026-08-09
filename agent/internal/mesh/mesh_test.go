package mesh

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()

	priv1, pub1, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if priv1 == "" || pub1 == "" {
		t.Fatal("empty keypair")
	}
	if priv1 == pub1 {
		t.Fatal("private and public key must differ")
	}

	// Second load must return the SAME identity — stable across restarts.
	priv2, pub2, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if priv1 != priv2 || pub1 != pub2 {
		t.Fatalf("keypair not stable: (%s,%s) then (%s,%s)", priv1, pub1, priv2, pub2)
	}

	// A different data dir yields a different identity.
	priv3, _, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if priv3 == priv1 {
		t.Fatal("distinct hosts generated identical keys")
	}
}

func TestRenderConfig(t *testing.T) {
	endpoint := "203.0.113.7:51820"
	got := RenderConfig("PRIV", "10.8.0.1", []Peer{
		{ServerID: "srv_b", Name: "host-b", Pubkey: "PUB_B", MeshIP: "10.8.0.2"},
		{ServerID: "srv_c", Name: "host-c", Pubkey: "PUB_C", MeshIP: "10.8.0.3", Endpoint: &endpoint},
	})

	for _, want := range []string{
		"PrivateKey = PRIV",
		"Address = 10.8.0.1/16",
		"PublicKey = PUB_B",
		"AllowedIPs = 10.8.0.2/32",
		"PublicKey = PUB_C",
		"AllowedIPs = 10.8.0.3/32",
		"Endpoint = 203.0.113.7:51820",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "[Peer]") != 2 {
		t.Fatalf("want 2 [Peer] sections:\n%s", got)
	}
	// Peer without an endpoint must not emit an Endpoint line.
	if strings.Count(got, "Endpoint = ") != 1 {
		t.Fatalf("want exactly 1 Endpoint line:\n%s", got)
	}
}

func TestWriteConfigChangeDetection(t *testing.T) {
	dir := t.TempDir()

	_, changed, err := WriteConfig(dir, "v1")
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	_, changed, err = WriteConfig(dir, "v1")
	if err != nil || changed {
		t.Fatalf("same content rewrite: changed=%v err=%v", changed, err)
	}
	_, changed, err = WriteConfig(dir, "v2")
	if err != nil || !changed {
		t.Fatalf("new content: changed=%v err=%v", changed, err)
	}
}

// The decommission's mesh step (SIGMA-204). The interface bring-down is
// Linux-and-wg-quick-only and cannot be exercised here, but the part that makes
// the teardown PERMANENT can: both the rendered config and the private key go,
// so an agent that somehow restarts cannot re-form the tunnel it was just
// removed from.
func TestTearDownRemovesConfigAndKey(t *testing.T) {
	dir := t.TempDir()
	priv, _, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteConfig(dir, RenderConfig(priv, "10.8.0.2", nil)); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := TearDown(context.Background(), log, dir); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, name := range []string{ConfigFile, "wg.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived the teardown (stat err = %v)", name, err)
		}
	}

	// Idempotent: the op may be re-delivered after a crash, and "no mesh here"
	// is the state it promises, not "a command ran".
	if err := TearDown(context.Background(), log, dir); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
}
