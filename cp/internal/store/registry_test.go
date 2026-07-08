package store

import (
	"encoding/json"
	"testing"
)

func TestNormalizeFacts(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "{}"},
		{"json null", "null", "{}"},
		{"scalar", "5", "{}"},
		{"array", "[1,2]", "{}"},
		{"string", `"hi"`, "{}"},
		{"object passthrough", `{"os":"linux"}`, `{"os":"linux"}`},
		{"padded object", "  {\"a\":1}  ", `{"a":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(normalizeFacts(json.RawMessage(tc.in)))
			if got != tc.want {
				t.Fatalf("normalizeFacts(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
