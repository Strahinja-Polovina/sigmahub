// Package deploy holds the compose file, the runbook and the smoke check that
// bring a SigmaHub control plane up on a real host — plus the tests that keep
// those files and the CI workflows that drive them agreeing.
//
// There is no Go source here on purpose: nothing in the control-plane binary
// reads these files, GitHub Actions and the staging box do, so the package
// exists only as a home for the checks. It is the same shape
// agent/packaging/install_script_test.go already uses for the installer: when a
// fact is written once in YAML and once in shell and neither can import the
// other, the copy is allowed and the DRIFT is not, so the copy gets a test that
// reads the other file off disk.
//
// What is pinned here is the DEPLOY GATE — which of the stack's paths a rollout
// has to prove before it is allowed to report success. That gate has a specific
// way of failing that no other suite can see: it keeps passing. A check that
// polls a URL nothing in the broken path serves answers 200 while the product
// is down, and a green deploy is worse than no deploy check at all, because it
// is the state in which nobody looks.
package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const deployStagingWorkflow = "../../.github/workflows/deploy-staging.yml"

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The staging rollout must prove the web→control-plane path before it reports
// success (SIGMA-265).
//
// The gate used to be `/readyz` on the control plane and `/` on the dashboard.
// `/readyz` pings Postgres and nothing else; `/` is the marketing home page,
// which for an anonymous curl renders static sections and never touches the
// control plane, SIGMAHUB_CP_URL or the service token. So the entire class of
// bug that breaks web→CP — the service-token env var renamed, SIGMAHUB_CP_URL
// dropped from the compose environment (which docker-compose.yml already
// records happening once, see the CP_HUGGING_FACE_TOKEN comment), the CP's auth
// middleware tightened — passed the gate with a green tick while every
// logged-in page on staging threw on its first control-plane call.
//
// Two things close it, and this test requires both because each covers what the
// other cannot: smoke.sh drives org → project → environment → enrollment-token
// against the real HTTP surface, and the dashboard health route proves that the
// WEB CONTAINER's own configuration reaches the control plane — a thing no
// curl from the host can observe.
func TestStagingRolloutIsGatedOnTheWebToControlPlanePath(t *testing.T) {
	wf := readFileForTest(t, deployStagingWorkflow)

	if !strings.Contains(wf, "smoke.sh") {
		t.Error("the staging deploy never runs cp/deploy/smoke.sh; the only artifact that " +
			"exercises org → project → environment → enrollment-token provisioning is " +
			"documented as a manual step and therefore never runs")
	}

	// The dashboard poll must name a route that round-trips to the control
	// plane. The bare marketing root is the failure this ticket is about.
	webPoll := regexp.MustCompile(`wait_for web (\S+)`).FindStringSubmatch(wf)
	if webPoll == nil {
		t.Fatal("no `wait_for web <url>` readiness poll in the staging rollout")
	}
	if strings.HasSuffix(webPoll[1], ":3000/") || strings.HasSuffix(webPoll[1], ":3000") {
		t.Errorf("the dashboard readiness gate polls %s — the marketing home page, which "+
			"renders for an anonymous request without touching the control plane, so it "+
			"answers 200 while every authenticated page is broken", webPoll[1])
	}

	// The route the workflow polls has to exist in the dashboard, or the gate
	// is a 404 that curl -sf would fail on for the wrong reason forever.
	if strings.Contains(webPoll[1], "/api/health") {
		if _, err := os.Stat("../../web/src/app/api/health/route.ts"); err != nil {
			t.Errorf("the rollout polls %s but web/src/app/api/health/route.ts does not exist: %v", webPoll[1], err)
		}
	}
}
