package integration

// Host facts over the real agent routes and a real database (SIGMA-201).
//
// The three things the product most needs to know about a machine — what
// distro it runs, how much disk it has, whether it has a GPU — now come from
// the machine instead of from a dropdown the operator filled in beforehand.
// That only works if two things hold at once, and neither can be shown without
// Postgres in the loop, because both are about what the `facts` JSONB column
// looks like after a sequence of check-ins:
//
//   - a current agent's readings survive register and every heartbeat, and
//     TRACK the host as it changes; and
//   - an agent too old to know about them keeps heartbeating and does NOT blank
//     them by omission. The fleet upgrades itself over hours; if the second
//     rule failed, every fact would flicker on and off at 30-second intervals
//     for as long as the rollout took, and the registration gate reading them
//     would refuse hosts at random.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// A full payload from a current agent: an Ubuntu 24.04 GPU box with two L40S.
const currentAgentFacts = `{
  "hostname": "gpu-1", "os": "linux", "arch": "amd64", "kernel": "6.8.0-45-generic",
  "numCpu": 32, "memTotalMb": 128744, "goVersion": "go1.26", "pid": 812,
  "dockerAvailable": true, "dockerVersion": "27.3.1",
  "distro": "ubuntu-24.04", "distroName": "Ubuntu 24.04.1 LTS",
  "diskTotalBytes": 1968526655488, "diskFreeBytes": 1801439850948, "diskPath": "/var/lib/sigmad",
  "gpu": {
    "vendor": "nvidia", "model": "NVIDIA L40S", "count": 2,
    "vramBytesPerGpu": 48301604864, "vramBytesTotal": 96603209728,
    "driverVersion": "550.54.15",
    "cards": [
      {"index": 0, "model": "NVIDIA L40S", "vramBytes": 48301604864},
      {"index": 1, "model": "NVIDIA L40S", "vramBytes": 48301604864}
    ]
  }
}`

// What an agent built before SIGMA-201 sends: not one of the new keys.
const oldAgentFacts = `{
  "hostname": "gpu-1", "os": "linux", "arch": "amd64", "kernel": "6.8.0-45-generic",
  "numCpu": 32, "memTotalMb": 128744, "goVersion": "go1.25", "pid": 4211,
  "dockerAvailable": true, "dockerVersion": "27.3.1"
}`

func TestAgentFactsRoundTrip(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_facts"

	// The operator's guess, made in the connect wizard before anyone had logged
	// into the box — and wrong, which is the whole point of asking the machine.
	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/provision", map[string]any{
		"name": "gpu-1", "type": "gpu", "distro": "ubuntu-22.04",
	})
	if code != http.StatusCreated {
		t.Fatalf("provision → %d: %v", code, body)
	}
	serverID, _ := body["serverId"].(string)
	bootTok, _ := body["token"].(string)

	code, body = postAs(t, ts, "", "/v1/agent/register", map[string]any{
		"bootstrapToken": bootTok,
		"name":           "gpu-1",
		"agentVersion":   "0.9.0",
		"facts":          json.RawMessage(currentAgentFacts),
	})
	if code != http.StatusCreated {
		t.Fatalf("register → %d: %v", code, body)
	}
	agentToken, _ := body["agentToken"].(string)
	if agentToken == "" {
		t.Fatalf("register returned no agent token: %v", body)
	}

	// Register persists the facts, and the machine's own answer replaces the
	// wizard's guess.
	srv := getServer(t, st, orgID, serverID)
	f := store.ParseHostFacts(srv.Facts)
	if f.Distro != "ubuntu-24.04" {
		t.Fatalf("facts.distro after register = %q, want ubuntu-24.04", f.Distro)
	}
	if srv.Distro == nil || *srv.Distro != "ubuntu-24.04" {
		t.Fatalf("servers.distro = %v, want the agent's ubuntu-24.04 over the wizard's ubuntu-22.04", srv.Distro)
	}
	if f.DiskTotalBytes != 1968526655488 {
		t.Fatalf("facts.diskTotalBytes after register = %d", f.DiskTotalBytes)
	}
	if f.GPU == nil || f.GPU.Count != 2 || f.GPU.VRAMBytesPerGPU != 48301604864 {
		t.Fatalf("facts.gpu after register = %+v", f.GPU)
	}

	// The same facts on the heartbeat path, which is where they are re-sent for
	// the rest of the server's life.
	code, body = postAs(t, ts, agentToken, "/v1/agent/heartbeat", map[string]any{
		"agentVersion": "0.9.0",
		"facts":        json.RawMessage(currentAgentFacts),
	})
	if code != http.StatusOK {
		t.Fatalf("heartbeat → %d: %v", code, body)
	}
	f = store.ParseHostFacts(getServer(t, st, orgID, serverID).Facts)
	if f.GPU == nil || f.GPU.VRAMBytesTotal != 96603209728 {
		t.Fatalf("facts.gpu after heartbeat = %+v", f.GPU)
	}

	// A host that CHANGES must be tracked, not frozen at what it looked like on
	// day one: the disk was grown and one card was pulled. The GPU inventory is
	// the case that would break if absence were the only way to say "none" —
	// here it is stated explicitly and has to take effect.
	const shrunkGPU = `{
	  "hostname": "gpu-1", "os": "linux", "arch": "amd64",
	  "distro": "ubuntu-24.04", "distroName": "Ubuntu 24.04.1 LTS",
	  "diskTotalBytes": 3937053310976, "diskFreeBytes": 3800000000000, "diskPath": "/var/lib/sigmad",
	  "gpu": {"vendor": "nvidia", "model": "NVIDIA L40S", "count": 1,
	          "vramBytesPerGpu": 48301604864, "vramBytesTotal": 48301604864,
	          "driverVersion": "550.54.15",
	          "cards": [{"index": 0, "model": "NVIDIA L40S", "vramBytes": 48301604864}]}
	}`
	code, body = postAs(t, ts, agentToken, "/v1/agent/heartbeat", map[string]any{
		"agentVersion": "0.9.0",
		"facts":        json.RawMessage(shrunkGPU),
	})
	if code != http.StatusOK {
		t.Fatalf("heartbeat → %d: %v", code, body)
	}
	f = store.ParseHostFacts(getServer(t, st, orgID, serverID).Facts)
	if f.DiskTotalBytes != 3937053310976 {
		t.Fatalf("grown disk not tracked: diskTotalBytes = %d", f.DiskTotalBytes)
	}
	if f.GPU == nil || f.GPU.Count != 1 || len(f.GPU.Cards) != 1 {
		t.Fatalf("pulled card not tracked: gpu = %+v", f.GPU)
	}

	// And a host that reports no GPU at all — a driver that stopped loading —
	// must be able to say so, or it keeps being scheduled model workloads.
	code, _ = postAs(t, ts, agentToken, "/v1/agent/heartbeat", map[string]any{
		"agentVersion": "0.9.0",
		"facts":        json.RawMessage(`{"os":"linux","gpu":{"vendor":"","count":0}}`),
	})
	if code != http.StatusOK {
		t.Fatalf("heartbeat → %d", code)
	}
	f = store.ParseHostFacts(getServer(t, st, orgID, serverID).Facts)
	if f.GPU == nil {
		t.Fatal("an explicit empty inventory was dropped; 'no GPU' must be storable")
	}
	if f.GPU.Count != 0 || f.GPU.Vendor != "" {
		t.Fatalf("lost GPU still reported as %+v", f.GPU)
	}
	// The unrelated facts in that minimal payload are untouched — the merge
	// preserves, it does not reset the row to whatever the last payload said.
	if f.Distro != "ubuntu-24.04" || f.DiskTotalBytes != 3937053310976 {
		t.Fatalf("merge lost unrelated facts: distro=%q disk=%d", f.Distro, f.DiskTotalBytes)
	}
}

// The compatibility rule, stated as a test: an agent that predates these facts
// heartbeats successfully and leaves everything already known about the host
// exactly as it was.
func TestOldAgentHeartbeatKeepsKnownFacts(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_facts_compat"

	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/provision", map[string]any{
		"name": "gpu-2", "type": "gpu", "distro": "ubuntu-24.04",
	})
	if code != http.StatusCreated {
		t.Fatalf("provision → %d: %v", code, body)
	}
	serverID, _ := body["serverId"].(string)
	bootTok, _ := body["token"].(string)
	code, body = postAs(t, ts, "", "/v1/agent/register", map[string]any{
		"bootstrapToken": bootTok, "name": "gpu-2", "agentVersion": "0.9.0",
		"facts": json.RawMessage(currentAgentFacts),
	})
	if code != http.StatusCreated {
		t.Fatalf("register → %d: %v", code, body)
	}
	agentToken, _ := body["agentToken"].(string)
	before := store.ParseHostFacts(getServer(t, st, orgID, serverID).Facts)

	// Every shape of "says nothing about the new facts" an agent can produce:
	// the pre-SIGMA-201 payload, an empty object, a payload whose probes came
	// back blank, one that nulls them out, and outright garbage.
	for _, payload := range []struct {
		name  string
		facts json.RawMessage
	}{
		{"pre-SIGMA-201 agent", json.RawMessage(oldAgentFacts)},
		{"empty object", json.RawMessage(`{}`)},
		{"probes returned nothing", json.RawMessage(`{"os":"linux","distro":"","diskTotalBytes":0,"diskPath":""}`)},
		{"explicit nulls", json.RawMessage(`{"os":"linux","distro":null,"gpu":null,"diskTotalBytes":null}`)},
	} {
		t.Run(payload.name, func(t *testing.T) {
			code, body := postAs(t, ts, agentToken, "/v1/agent/heartbeat", map[string]any{
				"agentVersion": "0.8.0",
				"facts":        payload.facts,
				"metrics":      map[string]any{"cpuPct": 12.5, "memPct": 40, "diskPct": 8, "load1": 0.4},
			})
			if code != http.StatusOK {
				t.Fatalf("heartbeat → %d: %v (an agent that omits these fields must keep checking in)", code, body)
			}
			srv := getServer(t, st, orgID, serverID)
			got := store.ParseHostFacts(srv.Facts)
			if got.Distro != before.Distro || got.DistroName != before.DistroName {
				t.Errorf("distro wiped: %q/%q, want %q/%q", got.Distro, got.DistroName, before.Distro, before.DistroName)
			}
			if got.DiskTotalBytes != before.DiskTotalBytes || got.DiskFreeBytes != before.DiskFreeBytes || got.DiskPath != before.DiskPath {
				t.Errorf("disk wiped: %+v, want %d/%d at %q",
					got, before.DiskTotalBytes, before.DiskFreeBytes, before.DiskPath)
			}
			if got.GPU == nil {
				t.Fatalf("GPU inventory wiped by a payload that simply did not mention it")
			}
			if got.GPU.Count != before.GPU.Count || got.GPU.VRAMBytesPerGPU != before.GPU.VRAMBytesPerGPU ||
				got.GPU.DriverVersion != before.GPU.DriverVersion || len(got.GPU.Cards) != len(before.GPU.Cards) {
				t.Errorf("GPU inventory changed: %+v, want %+v", *got.GPU, *before.GPU)
			}
			if srv.Distro == nil || *srv.Distro != "ubuntu-24.04" {
				t.Errorf("servers.distro = %v, want ubuntu-24.04 preserved", srv.Distro)
			}
			// The heartbeat is still a real heartbeat: liveness and metrics land.
			if srv.Status != "running" {
				t.Errorf("status = %q, want running", srv.Status)
			}
			if srv.LastSeenAt == nil {
				t.Error("last_seen_at not updated")
			}
		})
	}

	// A truncated payload cannot come in over HTTP — a facts value that is not
	// valid JSON makes the whole request body invalid and decodeJSON answers
	// 400 — so the store's own guarantee is asserted directly: garbage in must
	// leave the facts column standing rather than destroying it or failing the
	// jsonb cast, which is what it used to do.
	if err := st.RecordHeartbeat(context.Background(), serverID, store.HeartbeatInput{
		AgentVersion: "0.8.0",
		Facts:        json.RawMessage(`{"os":"lin`),
	}); err != nil {
		t.Fatalf("heartbeat with a truncated facts payload: %v", err)
	}
	if got := store.ParseHostFacts(getServer(t, st, orgID, serverID).Facts); got.Distro != before.Distro || got.GPU == nil {
		t.Fatalf("truncated payload damaged the facts: %+v", got)
	}

	// The old agent's OWN facts still update — merging preserves what a payload
	// omits, it does not freeze what the payload actually says.
	got := store.ParseHostFacts(getServer(t, st, orgID, serverID).Facts)
	if got.Arch != "amd64" {
		t.Fatalf("arch = %q", got.Arch)
	}
	var raw map[string]any
	if err := json.Unmarshal(getServer(t, st, orgID, serverID).Facts, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["goVersion"] != "go1.25" {
		t.Fatalf("goVersion = %v, want the old agent's own reported go1.25", raw["goVersion"])
	}
}

func getServer(t *testing.T, st *store.Store, orgID, serverID string) store.Server {
	t.Helper()
	srv, err := st.GetServer(context.Background(), orgID, serverID)
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	return srv
}
