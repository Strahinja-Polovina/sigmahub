package store

// The Go half of the cross-language parity guard for the compatibility gate.
//
// testdata/compat_cases.json is asserted here AND by
// web/src/lib/server-compat.test.ts. The control plane owns the decision; the
// dashboard re-implements it only so demo mode can show the state without
// anyone owning the wrong hardware. Two implementations of one rule drift
// silently — this file and its TypeScript twin make the drift loud.

import (
	"encoding/json"
	"os"
	"testing"
)

type compatFixture struct {
	Cases []struct {
		Name   string              `json:"name"`
		Type   string              `json:"type"`
		Facts  json.RawMessage     `json:"facts"`
		Expect []FailedRequirement `json:"expect"`
	} `json:"cases"`
}

func TestCompatibilityFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/compat_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx compatFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no fixtures — the parity guard would pass vacuously")
	}

	for _, tc := range fx.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			// ParseHostFacts, not a bespoke decoder: the fixtures are agent
			// payloads, and the gate must be shown reading them the way the
			// register and heartbeat paths do.
			got := CheckServerCompatibility(tc.Type, ParseHostFacts(tc.Facts))
			if len(got) != len(tc.Expect) {
				t.Fatalf("got %d failure(s), want %d\n got: %+v\nwant: %+v",
					len(got), len(tc.Expect), got, tc.Expect)
			}
			for i, want := range tc.Expect {
				if got[i] != want {
					t.Errorf("failure %d:\n got %+v\nwant %+v", i, got[i], want)
				}
			}
		})
	}
}
