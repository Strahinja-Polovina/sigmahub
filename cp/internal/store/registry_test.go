package store

import (
	"encoding/json"
	"fmt"
	"strings"
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

		// Facts are merged into the stored object, so a payload that says
		// nothing must BE nothing. Malformed JSON used to reach Postgres and
		// fail the whole heartbeat on the jsonb cast; now it collapses to {}
		// and leaves the stored facts alone.
		{"malformed object", `{"os":`, "{}"},
		{"trailing garbage", `{"os":"linux"} oops`, "{}"},

		// A fact reported as its zero value is a probe that could not answer.
		// Dropping it here is what makes the merge preserve the last real
		// reading instead of overwriting it with a blank.
		{"empty distro dropped", `{"distro":"","os":"linux"}`, `{"os":"linux"}`},
		// A zero TOTAL is a failed probe (no disk has zero capacity) and is
		// dropped; a zero FREE is a full disk, which is the reading most worth
		// keeping, so it survives alongside it.
		{"zero disk total dropped, zero free kept", `{"diskTotalBytes":0,"diskFreeBytes":0,"diskPath":"","os":"linux"}`, `{"diskFreeBytes":0,"os":"linux"}`},
		{"real disk kept", `{"diskTotalBytes":512110190592}`, `{"diskTotalBytes":512110190592}`},
		{"null of any fact dropped", `{"gpu":null,"kernel":null,"os":"linux"}`, `{"os":"linux"}`},

		// The one fact whose zero value is a real answer: "I looked and this
		// host has no GPU" must be storable, or a machine that loses its card
		// advertises it forever.
		{"empty gpu inventory kept", `{"gpu":{"vendor":"","count":0}}`, `{"gpu":{"vendor":"","count":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(normalizeFacts(json.RawMessage(tc.in)))
			if got != tc.want {
				t.Fatalf("normalizeFacts(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Zero free space is the one disk reading worth acting on, and it is reachable:
// gopsutil reports the unprivileged-available figure, which hits zero on ext4
// while the root reserve still has room. It used to be stripped as "the probe
// could not answer", and because facts MERGE, a full disk then kept advertising
// its last healthy free-space figure indefinitely.
func TestNormalizeFactsKeepsAGenuineZeroFree(t *testing.T) {
	got := string(normalizeFacts([]byte(`{"diskTotalBytes":512,"diskFreeBytes":0,"os":"linux"}`)))
	if !strings.Contains(got, `"diskFreeBytes":0`) {
		t.Fatalf("normalizeFacts = %s, want the zero free reading kept", got)
	}
	// A zero TOTAL is still a failed probe — no disk has zero capacity.
	got = string(normalizeFacts([]byte(`{"diskTotalBytes":0,"os":"linux"}`)))
	if strings.Contains(got, "diskTotalBytes") {
		t.Fatalf("normalizeFacts = %s, want a zero total dropped", got)
	}
}

// Docker being removed has to clear the version. Facts merge, so a key that is
// merely omitted keeps its old value — the dashboard went on showing the
// version of a daemon that was no longer installed.
func TestNormalizeFactsKeepsAnEmptyDockerVersion(t *testing.T) {
	got := string(normalizeFacts([]byte(`{"dockerAvailable":false,"dockerVersion":""}`)))
	if !strings.Contains(got, `"dockerVersion":""`) {
		t.Fatalf("normalizeFacts = %s, want the cleared version kept so the merge overwrites", got)
	}
}

// A merge never removes a key, so without a bound an agent-token holder can add
// one key per heartbeat until the jsonb cell reaches Postgres' 1 GB field limit
// and that server can never heartbeat again. Assignment used to bound this
// implicitly; the merge that fixed version skew removed the bound.
func TestNormalizeFactsRefusesAnUnboundedPayload(t *testing.T) {
	var wide strings.Builder
	wide.WriteString(`{`)
	for i := 0; i < maxFactKeys+1; i++ {
		if i > 0 {
			wide.WriteString(",")
		}
		fmt.Fprintf(&wide, `"k%d":%d`, i, i)
	}
	wide.WriteString(`}`)
	if got := string(normalizeFacts([]byte(wide.String()))); got != "{}" {
		t.Fatalf("a payload with too many keys normalized to %s, want {}", got)
	}

	big := fmt.Sprintf(`{"os":%q}`, strings.Repeat("x", maxFactBytes))
	if got := string(normalizeFacts([]byte(big))); got != "{}" {
		t.Fatalf("an oversized payload normalized to %d bytes, want {}", len(got))
	}

	// An ordinary payload is untouched — the bound must not cost a real host
	// its facts.
	ok := `{"os":"linux","arch":"amd64","distro":"ubuntu-24.04"}`
	if got := string(normalizeFacts([]byte(ok))); !strings.Contains(got, "ubuntu-24.04") {
		t.Fatalf("a normal payload was refused: %s", got)
	}
}
