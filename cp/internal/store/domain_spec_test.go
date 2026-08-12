package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// SIGMA-358: a resource spec must never be run through the host-facts normalizer,
// which collapses anything over 16 KB or 64 keys to {} and strips zero-valued
// fact keys. A large but valid Compose spec silently became {} and then never
// deployed. normalizeSpec keeps a valid spec byte-for-byte and rejects an
// unusable one loudly.
func TestNormalizeSpec(t *testing.T) {
	// A large, valid app spec (well past the 16 KB facts cap) round-trips intact.
	services := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		services = append(services, map[string]any{
			"name":  "svc-" + strings.Repeat("x", 200),
			"image": "ghcr.io/acme/service:" + strings.Repeat("y", 200),
			"env":   map[string]string{"KEY_" + strings.Repeat("z", 100): strings.Repeat("v", 300)},
		})
	}
	big, _ := json.Marshal(map[string]any{"compose": map[string]any{"services": services}})
	if len(big) <= 16<<10 {
		t.Fatalf("precondition: test spec must exceed the 16 KB facts cap, got %d bytes", len(big))
	}
	out, err := normalizeSpec(big)
	if err != nil {
		t.Fatalf("a large valid spec must be accepted, got %v", err)
	}
	if len(out) != len(big) {
		t.Fatalf("a valid spec must be stored byte-for-byte, not collapsed (got %d bytes from %d)", len(out), len(big))
	}

	// Empty / nil / whitespace become an empty object, never an error.
	for _, in := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("  ")} {
		out, err := normalizeSpec(in)
		if err != nil || string(out) != "{}" {
			t.Fatalf("empty spec = (%q, %v), want ({}, nil)", out, err)
		}
	}

	// A non-object or malformed spec is refused with a typed error, not silently
	// wiped to {}.
	for _, in := range []string{`[1,2,3]`, `"a string"`, `42`, `{not json}`} {
		_, err := normalizeSpec(json.RawMessage(in))
		if err == nil {
			t.Fatalf("spec %q must be rejected, not accepted", in)
		}
		var inv ErrInvalid
		if !errors.As(err, &inv) {
			t.Fatalf("spec %q rejection = %#v, want ErrInvalid", in, err)
		}
	}

	// An implausibly large spec is refused rather than stored.
	huge := json.RawMessage(`{"x":"` + strings.Repeat("q", maxSpecBytes) + `"}`)
	if _, err := normalizeSpec(huge); err == nil {
		t.Fatal("a spec past maxSpecBytes must be rejected")
	}
}
