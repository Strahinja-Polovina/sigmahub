package integration

// The registration compatibility gate, end to end (SIGMA-203).
//
// Every assertion here goes through the real HTTP routes into real Postgres,
// because the gate is not a function — it is a sequence of states a row moves
// through as an agent talks to the control plane. The three properties that
// matter are all invisible to a unit test of the checker:
//
//   - a host that does not meet its type's requirements lands in a status that
//     is neither running (billed, scheduled) nor provisioning (a spinner that
//     never resolves), carrying the reasons the dashboard renders;
//   - an agent that says NOTHING about a requirement is not refused for it. The
//     fleet upgrades over hours, and a gate that treated silence as violation
//     would mark every host in it incompatible on deploy day;
//   - the state is an exit, not a trap: a later heartbeat that satisfies the
//     requirement recovers the server, and re-filing it under a type it does
//     satisfy clears it immediately.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Facts fixtures. Each is a whole payload from a current agent, differing from
// the compatible one in exactly the datum under test — a fixture that drifted
// in two places at once would make it impossible to say which check fired.
const (
	gpuHostFacts = `{
	  "hostname": "gpu-box", "os": "linux", "arch": "amd64", "numCpu": 32, "memTotalMb": 128744,
	  "distro": "ubuntu-24.04", "distroName": "Ubuntu 24.04.1 LTS",
	  "diskTotalBytes": 1968526655488, "diskFreeBytes": 1801439850948, "diskPath": "/var/lib/sigmad",
	  "gpu": {"vendor": "nvidia", "model": "NVIDIA L40S", "count": 2,
	          "vramBytesPerGpu": 48301604864, "vramBytesTotal": 96603209728,
	          "driverVersion": "550.54.15",
	          "cards": [{"index": 0, "model": "NVIDIA L40S", "vramBytes": 48301604864},
	                    {"index": 1, "model": "NVIDIA L40S", "vramBytes": 48301604864}]}
	}`

	// The same box with no accelerator. The inventory is PRESENT and empty —
	// the agent looked and found nothing — which is the reading the gate acts
	// on, as opposed to the absent key an old agent sends.
	noGPUHostFacts = `{
	  "hostname": "plain-box", "os": "linux", "arch": "amd64", "numCpu": 8, "memTotalMb": 32768,
	  "distro": "ubuntu-24.04", "distroName": "Ubuntu 24.04.1 LTS",
	  "diskTotalBytes": 1968526655488, "diskPath": "/var/lib/sigmad",
	  "gpu": {"vendor": "", "count": 0}
	}`

	// A GPU box on the wrong CPU architecture: the inference runtime we pin
	// publishes x86_64 layers only, so this enrolls happily and then cannot
	// pull its image.
	armGPUHostFacts = `{
	  "hostname": "gh200", "os": "linux", "arch": "arm64", "numCpu": 72,
	  "distro": "ubuntu-24.04", "distroName": "Ubuntu 24.04.1 LTS",
	  "diskTotalBytes": 1968526655488, "diskPath": "/var/lib/sigmad",
	  "gpu": {"vendor": "nvidia", "model": "NVIDIA GH200", "count": 1,
	          "vramBytesPerGpu": 102005473280, "driverVersion": "550.54.15",
	          "cards": [{"index": 0, "model": "NVIDIA GH200", "vramBytes": 102005473280}]}
	}`

	// 120 GB — an ordinary VPS someone filed as object storage.
	smallDiskHostFacts = `{
	  "hostname": "vps-3", "os": "linux", "arch": "amd64", "numCpu": 2, "memTotalMb": 4096,
	  "distro": "ubuntu-22.04", "distroName": "Ubuntu 22.04.4 LTS",
	  "diskTotalBytes": 120000000000, "diskPath": "/", "gpu": {"vendor": "", "count": 0}
	}`

	// A distro outside the onboardable set, on an otherwise fine machine.
	fedoraHostFacts = `{
	  "hostname": "workstation", "os": "linux", "arch": "amd64", "numCpu": 16,
	  "distro": "fedora-41", "distroName": "Fedora Linux 41 (Workstation Edition)",
	  "diskTotalBytes": 1968526655488, "diskPath": "/", "gpu": {"vendor": "", "count": 0}
	}`

	// A card that enumerates with no driver behind it. It fails later and more
	// expensively than a missing card: the host enrolls, bills at GPU weight,
	// and dies at the first container start.
	driverlessGPUFacts = `{
	  "hostname": "gpu-nodriver", "os": "linux", "arch": "amd64", "numCpu": 32,
	  "distro": "ubuntu-24.04", "distroName": "Ubuntu 24.04.1 LTS",
	  "diskTotalBytes": 1968526655488, "diskPath": "/",
	  "gpu": {"vendor": "nvidia", "model": "NVIDIA L40S", "count": 1, "vramBytesPerGpu": 48301604864,
	          "cards": [{"index": 0, "model": "NVIDIA L40S", "vramBytes": 48301604864}]}
	}`
)

// sendAs drives any method; postAs (server_catalog_integration_test.go) covers
// POST, and the disconnect exit is a DELETE.
func sendAs(t *testing.T, ts *httptest.Server, method, token, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// connectAgent provisions a server of the given type and registers an agent
// against it with the supplied facts — the exact sequence the installer runs.
// Returns the server id and the agent token.
func connectAgent(t *testing.T, ts *httptest.Server, svcToken, orgID, name, typ, facts string) (string, string) {
	t.Helper()
	body := map[string]any{"type": typ, "hostIp": "203.0.113.9"}
	if name != "" {
		body["name"] = name
	}
	code, out := postAs(t, ts, svcToken, "/v1/orgs/"+orgID+"/servers/provision", body)
	if code != http.StatusCreated {
		t.Fatalf("provision %s → %d: %v", typ, code, out)
	}
	serverID, _ := out["serverId"].(string)
	bootTok, _ := out["token"].(string)

	code, out = postAs(t, ts, "", "/v1/agent/register", map[string]any{
		"bootstrapToken": bootTok,
		"agentVersion":   "0.9.0",
		"facts":          json.RawMessage(facts),
	})
	if code != http.StatusCreated {
		t.Fatalf("register → %d: %v", code, out)
	}
	agentToken, _ := out["agentToken"].(string)
	if agentToken == "" {
		t.Fatalf("register returned no agent token: %v", out)
	}
	return serverID, agentToken
}

func heartbeat(t *testing.T, ts *httptest.Server, agentToken, facts string) {
	t.Helper()
	code, out := postAs(t, ts, agentToken, "/v1/agent/heartbeat", map[string]any{
		"agentVersion": "0.9.0",
		"facts":        json.RawMessage(facts),
	})
	if code != http.StatusOK {
		t.Fatalf("heartbeat → %d: %v", code, out)
	}
}

// Every requirement in the catalog, failing and passing, over the real routes.
func TestRegistrationGateOnEachRequirement(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_gate"

	for _, tc := range []struct {
		name string
		typ  string
		// bad violates exactly one requirement of typ; good satisfies all of
		// them, so each row also proves the check does not fire on a host that
		// meets it — a gate that refused everything would pass the first half.
		bad, good string
		wantID    store.RequirementID
		wantFact  string
		// wantReason is asserted in full: the dashboard renders this string
		// verbatim, so a change to it is a product change and should break a
		// test rather than quietly reword what an operator reads.
		wantReason   string
		wantDetected string
	}{
		{
			name: "gpu", typ: "gpu", bad: noGPUHostFacts, good: gpuHostFacts,
			wantID: store.ReqGPU, wantFact: "gpu",
			wantReason:   "You connected this as a GPU server, but no NVIDIA GPU was detected.",
			wantDetected: "none",
		},
		{
			name: "gpu driver", typ: "gpu", bad: driverlessGPUFacts, good: gpuHostFacts,
			wantID: store.ReqGPU, wantFact: "gpu",
			wantReason:   "You connected this as a GPU server, but its NVIDIA GPU has no usable driver.",
			wantDetected: "1 × nvidia, no driver",
		},
		{
			name: "arch", typ: "gpu", bad: armGPUHostFacts, good: gpuHostFacts,
			wantID: store.ReqArch, wantFact: "arch",
			wantReason:   "You connected this as a GPU server, but its CPU architecture is arm64 — that type needs amd64.",
			wantDetected: "arm64",
		},
		{
			name: "disk", typ: "storage", bad: smallDiskHostFacts, good: noGPUHostFacts,
			wantID: store.ReqDisk, wantFact: "diskTotalBytes",
			wantReason:   "You connected this as a Storage server, but it has 120 GB of disk — that type needs at least 500 GB.",
			wantDetected: "120 GB",
		},
		{
			name: "distro", typ: "general", bad: fedoraHostFacts, good: noGPUHostFacts,
			wantID: store.ReqDistro, wantFact: "distro",
			wantReason: "You connected this as a General server, but it runs Fedora Linux 41 (Workstation Edition) — " +
				"that type needs Ubuntu 22.04 LTS, Ubuntu 24.04 LTS or Debian 12.",
			wantDetected: "Fedora Linux 41 (Workstation Edition)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badID, _ := connectAgent(t, ts, token, orgID, "bad-"+tc.name, tc.typ, tc.bad)
			srv := getServer(t, st, orgID, badID)
			if srv.Status != store.ServerStatusIncompatible {
				t.Fatalf("status = %q, want %q", srv.Status, store.ServerStatusIncompatible)
			}
			if len(srv.IncompatibleReasons) != 1 {
				t.Fatalf("reasons = %+v, want exactly the one violated requirement", srv.IncompatibleReasons)
			}
			got := srv.IncompatibleReasons[0]
			if got.ID != tc.wantID {
				t.Errorf("requirement id = %q, want %q", got.ID, tc.wantID)
			}
			if got.Fact != tc.wantFact {
				t.Errorf("fact = %q, want %q", got.Fact, tc.wantFact)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason =\n  %q\nwant\n  %q", got.Reason, tc.wantReason)
			}
			if got.Detected != tc.wantDetected {
				t.Errorf("detected = %q, want %q", got.Detected, tc.wantDetected)
			}
			// Expected quotes the catalog's own sentence — the same one the
			// connect dialog showed BEFORE the install, so the promise and the
			// refusal cannot word the requirement differently.
			spec, _ := store.ServerTypeSpecFor(tc.typ)
			var catalogText string
			for _, req := range spec.Requires.List() {
				if req.ID == tc.wantID {
					catalogText = req.Text
				}
			}
			if got.Expected != catalogText || catalogText == "" {
				t.Errorf("expected = %q, want the catalog's %q", got.Expected, catalogText)
			}

			// The passing half: same type, a host that meets the requirement.
			goodID, _ := connectAgent(t, ts, token, orgID, "good-"+tc.name, tc.typ, tc.good)
			good := getServer(t, st, orgID, goodID)
			if good.Status == store.ServerStatusIncompatible {
				t.Fatalf("a compatible %s host was refused: %+v", tc.typ, good.IncompatibleReasons)
			}
			if len(good.IncompatibleReasons) != 0 {
				t.Fatalf("compatible host carries reasons: %+v", good.IncompatibleReasons)
			}
		})
	}
}

// Several violations at once are all reported, in catalog order. An operator
// deciding whether to keep the machine needs the whole list — being told about
// the distro, fixing it, and only then learning about the disk is the same
// drip-feed the gate exists to end.
func TestGateReportsEveryViolatedRequirement(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)

	// Fedora, 120 GB, filed as storage: distro AND disk.
	const fedoraSmallDisk = `{
	  "hostname": "wrong-on-both", "os": "linux", "arch": "amd64",
	  "distro": "fedora-41", "distroName": "Fedora Linux 41",
	  "diskTotalBytes": 120000000000, "diskPath": "/", "gpu": {"vendor": "", "count": 0}
	}`
	serverID, _ := connectAgent(t, ts, token, "org_multi", "", "storage", fedoraSmallDisk)
	srv := getServer(t, st, "org_multi", serverID)
	if len(srv.IncompatibleReasons) != 2 {
		t.Fatalf("reasons = %+v, want distro and disk", srv.IncompatibleReasons)
	}
	if srv.IncompatibleReasons[0].ID != store.ReqDistro || srv.IncompatibleReasons[1].ID != store.ReqDisk {
		t.Fatalf("order = %q, %q; want the catalog's distro-then-disk so the gate and the "+
			"connect dialog's checklist read alike",
			srv.IncompatibleReasons[0].ID, srv.IncompatibleReasons[1].ID)
	}
}

// The compatibility rule that protects the existing fleet: an agent that never
// mentions a fact is not refused for it. On the day this ships most agents in a
// customer's fleet predate these facts entirely.
func TestOldAgentIsNeverMarkedIncompatible(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_oldagent"

	// The most demanding type in the catalog, so nothing is passing by luck.
	serverID, agentToken := connectAgent(t, ts, token, orgID, "legacy-gpu", "gpu", oldAgentFacts)
	srv := getServer(t, st, orgID, serverID)
	if srv.Status == store.ServerStatusIncompatible {
		t.Fatalf("an agent that reports none of these facts was refused: %+v", srv.IncompatibleReasons)
	}

	// And it stays that way across heartbeats, in every shape "says nothing"
	// takes on the wire.
	for _, facts := range []string{
		oldAgentFacts,
		`{}`,
		`{"os":"linux","distro":"","diskTotalBytes":0}`,
		`{"os":"linux","distro":null,"gpu":null,"diskTotalBytes":null}`,
	} {
		heartbeat(t, ts, agentToken, facts)
		srv := getServer(t, st, orgID, serverID)
		if srv.Status != store.ServerStatusRunning {
			t.Fatalf("status after %q = %q, want running; reasons %+v", facts, srv.Status, srv.IncompatibleReasons)
		}
	}

	// A host that only ever reported its ARCH is judged on that alone: the
	// requirement it can answer applies, the ones it cannot do not.
	badArch, _ := connectAgent(t, ts, token, orgID, "legacy-arm", "gpu", `{"hostname":"arm-1","arch":"arm64"}`)
	srv = getServer(t, st, orgID, badArch)
	if srv.Status != store.ServerStatusIncompatible || len(srv.IncompatibleReasons) != 1 ||
		srv.IncompatibleReasons[0].ID != store.ReqArch {
		t.Fatalf("a partially-reporting agent should fail on exactly what it DID report: %q %+v",
			srv.Status, srv.IncompatibleReasons)
	}
}

// The state is recoverable without re-enrolling: install the driver, and the
// next check-in brings the server back. This is also where "incompatible is not
// running" earns its keep — the host is not billed while it is refused.
func TestIncompatibleServerRecoversOnLaterHeartbeat(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	ts, token := catalogAPI(t, st)
	orgID := "org_recover"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "gpu-1", "gpu", driverlessGPUFacts)
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusIncompatible {
		t.Fatalf("status = %q, want incompatible", srv.Status)
	}

	// Heartbeating with the SAME bad facts must not launder it into running:
	// the old status CASE promoted anything that was not already running.
	heartbeat(t, ts, agentToken, driverlessGPUFacts)
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusIncompatible {
		t.Fatalf("status after an unchanged heartbeat = %q, want it to stay incompatible", srv.Status)
	}
	if _, servers, units, err := st.ConnectedServerUnits(ctx, orgID); err != nil {
		t.Fatal(err)
	} else if servers != 0 || units != 0 {
		t.Fatalf("an incompatible server is billed: %d server(s), %d unit(s)", servers, units)
	}

	// The operator installs the driver; the agent reports it on its next tick.
	heartbeat(t, ts, agentToken, gpuHostFacts)
	srv := getServer(t, st, orgID, serverID)
	if srv.Status != store.ServerStatusRunning {
		t.Fatalf("status after the fix = %q, want running", srv.Status)
	}
	if len(srv.IncompatibleReasons) != 0 {
		t.Fatalf("recovered server still carries reasons: %+v", srv.IncompatibleReasons)
	}
	if _, servers, units, err := st.ConnectedServerUnits(ctx, orgID); err != nil {
		t.Fatal(err)
	} else if servers != 1 || units != store.ServerUnitWeight("gpu") {
		t.Fatalf("recovered server bills as %d unit(s) over %d server(s), want %d over 1",
			units, servers, store.ServerUnitWeight("gpu"))
	}

	// And the other direction: the card is pulled. A host that can no longer do
	// the job stops being billed and scheduled for it.
	heartbeat(t, ts, agentToken, noGPUHostFacts)
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusIncompatible {
		t.Fatalf("status after the card was pulled = %q, want incompatible", srv.Status)
	}
}

// Exit 1: change the type. The machine is fine — it is the type it was filed
// under that is wrong — so the product's answer must keep the machine.
func TestChangeTypeExitClearsIncompatible(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_retype"

	// An ordinary 120 GB VPS: fails `gpu` (no card), fails `storage` (disk
	// floor), and is a perfectly good `general` server. One machine, three
	// verdicts — which is the point of the exit.
	serverID, agentToken := connectAgent(t, ts, token, orgID, "misfiled", "gpu", smallDiskHostFacts)
	heartbeat(t, ts, agentToken, smallDiskHostFacts) // it is live, just misfiled
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusIncompatible {
		t.Fatalf("status = %q, want incompatible", srv.Status)
	}

	// Re-filing it as a type it does NOT satisfy either answers 200 with the
	// new verdict rather than pretending the change worked.
	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/type",
		map[string]any{"type": "storage"})
	if code != http.StatusOK {
		t.Fatalf("type → storage → %d: %v", code, body)
	}
	if got, _ := body["status"].(string); got != store.ServerStatusIncompatible {
		t.Fatalf("response status = %q, want the endpoint to report the NEW verdict", got)
	}
	srv := getServer(t, st, orgID, serverID)
	if len(srv.IncompatibleReasons) != 1 || srv.IncompatibleReasons[0].ID != store.ReqDisk {
		t.Fatalf("reasons after re-filing = %+v, want the storage disk floor", srv.IncompatibleReasons)
	}

	// And as a type it does satisfy: cleared, and back to running because the
	// agent has already checked in — the operator does not wait for a
	// heartbeat to find out whether the exit worked.
	code, body = postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/type",
		map[string]any{"type": "general"})
	if code != http.StatusOK {
		t.Fatalf("type → general → %d: %v", code, body)
	}
	if got, _ := body["status"].(string); got != store.ServerStatusRunning {
		t.Fatalf("response status = %q, want running", got)
	}
	reasons, _ := body["incompatibleReasons"].([]any)
	if len(reasons) != 0 {
		t.Fatalf("response still carries reasons: %v", reasons)
	}
	srv = getServer(t, st, orgID, serverID)
	if srv.Type != "general" || srv.Status != store.ServerStatusRunning || len(srv.IncompatibleReasons) != 0 {
		t.Fatalf("stored row = %q/%q %+v, want a running general server with no reasons",
			srv.Type, srv.Status, srv.IncompatibleReasons)
	}

	// A type outside the catalog is refused at the boundary, not stored.
	if code, _ := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/type",
		map[string]any{"type": "toaster"}); code != http.StatusBadRequest {
		t.Fatalf("unknown type → %d, want 400", code)
	}
}

// Exit 2: disconnect. Deletion has never been gated on status, but the exit is
// only real if it works from the state it is offered in — and the incompatible
// server that reaches it is one nothing was ever scheduled onto.
func TestDisconnectExitWorksOnIncompatibleServer(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_disconnect"

	serverID, _ := connectAgent(t, ts, token, orgID, "doomed", "gpu", noGPUHostFacts)
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusIncompatible {
		t.Fatalf("status = %q, want incompatible", srv.Status)
	}
	if code, body := sendAs(t, ts, "DELETE", token, "/v1/orgs/"+orgID+"/servers/"+serverID, nil); code != http.StatusOK {
		t.Fatalf("disconnect → %d: %v", code, body)
	}
	if _, err := st.GetServer(context.Background(), orgID, serverID); err == nil {
		t.Fatal("disconnected server is still readable")
	}
}

// Nothing may be scheduled onto a refused host. Without this the gate's finding
// is advisory: the deploy is accepted, the container starts on hardware that
// cannot run it, and the operator debugs a rollout instead of reading a
// sentence about their server.
func TestIncompatibleServerRefusesResources(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	ts, token := catalogAPI(t, st)
	orgID := "org_noresources"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "gpu-bad", "gpu", noGPUHostFacts)
	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "test"); err != nil {
		t.Fatal(err)
	}
	_, err = st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "model", Kind: "llm",
	}, "test")
	var invalid store.ErrInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("create on an incompatible server: %v, want a domain-rule refusal", err)
	}
	if !strings.Contains(invalid.Msg, "incompatible") {
		t.Fatalf("refusal %q does not say why", invalid.Msg)
	}

	// Once the host recovers, the same create succeeds — the guard is on the
	// state, not on the server forever.
	heartbeat(t, ts, agentToken, gpuHostFacts)
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "model", Kind: "llm",
	}, "test"); err != nil {
		t.Fatalf("create after recovery: %v", err)
	}

	// And a server with work on it cannot be re-filed under a type that cannot
	// host that work — the exit must not strand what is already running.
	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/type",
		map[string]any{"type": "storage"})
	if code != http.StatusConflict {
		t.Fatalf("re-filing a server hosting an LLM as storage → %d: %v", code, body)
	}
	if msg, _ := body["error"].(string); !bytes.Contains([]byte(msg), []byte("model")) {
		t.Fatalf("conflict %q does not name the resource that blocks it", msg)
	}
}

// SIGMA-202: the connect form asks for an address and a type. The name it used
// to demand is the one thing on that form the machine already knew.
func TestRegistrationNamesServerFromReportedHostname(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_naming"

	// No name in the provision request at all.
	code, out := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/provision", map[string]any{
		"type": "general", "hostIp": "203.0.113.9",
	})
	if code != http.StatusCreated {
		t.Fatalf("provision → %d: %v", code, out)
	}
	serverID, _ := out["serverId"].(string)
	bootTok, _ := out["token"].(string)

	// Before the agent lands the row is still identifiable: it is named for the
	// address the operator typed, which is the only handle they have on it.
	if srv := getServer(t, st, orgID, serverID); srv.Name != "203.0.113.9" {
		t.Fatalf("pre-registration name = %q, want the host address as a placeholder", srv.Name)
	}

	code, out = postAs(t, ts, "", "/v1/agent/register", map[string]any{
		"bootstrapToken": bootTok, "agentVersion": "0.9.0",
		"facts": json.RawMessage(noGPUHostFacts),
	})
	if code != http.StatusCreated {
		t.Fatalf("register → %d: %v", code, out)
	}
	if srv := getServer(t, st, orgID, serverID); srv.Name != "plain-box" {
		t.Fatalf("name after registration = %q, want the reported hostname", srv.Name)
	}

	// Renamable afterwards, and the rename sticks: an operator's choice is not
	// something a later registration gets to overwrite.
	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/rename",
		map[string]any{"name": "hel-general-02"})
	if code != http.StatusOK {
		t.Fatalf("rename → %d: %v", code, body)
	}
	if got, _ := body["name"].(string); got != "hel-general-02" {
		t.Fatalf("rename response name = %q", got)
	}
	if srv := getServer(t, st, orgID, serverID); srv.Name != "hel-general-02" {
		t.Fatalf("stored name = %q", srv.Name)
	}
	if code, _ := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/rename",
		map[string]any{"name": "   "}); code != http.StatusUnprocessableEntity {
		t.Fatalf("blank rename → %d, want 422", code)
	}

	// A name the operator DID type survives registration untouched.
	named, _ := connectAgent(t, ts, token, orgID, "chosen-name", "general", noGPUHostFacts)
	if srv := getServer(t, st, orgID, named); srv.Name != "chosen-name" {
		t.Fatalf("operator-chosen name = %q, want it kept over the reported hostname", srv.Name)
	}
}

// The incompatible verdict is written to the audit log with its reason, so the
// question "why did this server never come up" has an answer after the fact.
func TestIncompatibilityIsAudited(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)
	orgID := "org_audit_compat"

	connectAgent(t, ts, token, orgID, "gpu-bad", "gpu", noGPUHostFacts)
	entries, err := st.ListAudit(context.Background(), orgID, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := "Server incompatible — You connected this as a GPU server, but no NVIDIA GPU was detected."
	for _, e := range entries {
		if e.Action == want {
			return
		}
	}
	t.Fatalf("no audit entry %q in %+v", want, entries)
}
