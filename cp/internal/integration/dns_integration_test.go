package integration

// SIGMA-299 / SIGMA-316 live here rather than in the store package for a
// concrete reason. These are the first DNS tests that need a real database,
// and `go test ./...` runs packages in PARALLEL: the integration harness
// begins every test with an unscoped `DELETE FROM servers` (and forty
// siblings) to get isolated state, so a store-package test seeding its own
// server into the same CP_TEST_DATABASE_URL races that wipe and fails on
// resources_server_id_fkey. It reproduces as a flake — green alone, red under
// ./... — which is the worst shape for a test to have.
//
// The pure functions (isApexDomain, apexOf, publicHost) stay in the store
// package where they belong; only the DB-backed cases moved.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// SIGMA-299: DNS pointing at a server that terminates no TLS is not "done".
//
// The reconciler renders Traefik onto a server ONLY when that server carries
// the proxy/edge role, so on a non-proxy host there is no ACME client and no
// HTTP-01 responder — the certificate can never issue, no matter how correct
// the A record is. The DNS setup panel used to mention the missing role only
// inside its "resolves, but not to this server" branch, so the one case where
// the role is the ONLY thing left blocking issuance rendered as a bare green
// tick with an empty reason: a terminal dead end with nothing in the product
// naming the cause.
//
// The fixture resolves for real: the domain is `localhost` and the server's
// endpoint is 127.0.0.1, so the verification probe genuinely matches the
// target and Verified comes back true — which is the whole point. What must
// also come back is a reason naming the server and the role to turn on.
func TestDNSSetupForDomain_ResolvesToNonProxyServer(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	const serverName = "web-03"
	ids := seedDNSFixture(t, ctx, st, serverName, "localhost", "127.0.0.1:51820", false)

	setup, err := st.DNSSetupForDomain(ctx, ids.orgID, ids.domainID)
	if err != nil {
		t.Fatalf("DNSSetupForDomain: %v", err)
	}
	if !setup.Verified {
		t.Fatalf("fixture did not resolve to its target (observed %v, records %+v) — "+
			"the non-proxy case this test exists for was never reached", setup.Observed, setup.Records)
	}
	if setup.Reason == "" {
		t.Fatalf("DNS resolves to a server with proxy_role=false and Reason is empty: the "+
			"dashboard renders a green tick while the certificate can never issue (setup %+v)", setup)
	}
	if !strings.Contains(setup.Reason, serverName) {
		t.Errorf("Reason %q does not name the server whose role is missing — the operator has "+
			"to be told WHICH server to change", setup.Reason)
	}
	if setup.ProxyRole {
		t.Errorf("ProxyRole = true for a server with proxy_role=false")
	}
}

// A proxy-role server that resolves correctly is the fully-good state: green
// tick, nothing to explain. Guards the fix against turning every verified
// domain into a warning.
func TestDNSSetupForDomain_ResolvesToProxyServer(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	ids := seedDNSFixture(t, ctx, st, "edge-01", "localhost", "127.0.0.1:51820", true)

	setup, err := st.DNSSetupForDomain(ctx, ids.orgID, ids.domainID)
	if err != nil {
		t.Fatalf("DNSSetupForDomain: %v", err)
	}
	if !setup.Verified {
		t.Fatalf("fixture did not resolve to its target (observed %v)", setup.Observed)
	}
	if !setup.ProxyRole {
		t.Fatalf("ProxyRole = false for a server with proxy_role=true")
	}
	if setup.Reason != "" {
		t.Fatalf("Reason = %q for a correctly-pointed domain on an edge server; this state has "+
			"nothing to explain", setup.Reason)
	}
}

type dnsFixtureIDs struct {
	orgID    string
	domainID string
}

// seedDNSFixture writes the smallest org/project/environment/server/resource/
// domain chain DNSSetupForDomain reads, and removes it again on cleanup. The
// domain name is global (domains are uniquely indexed on lower(domain)), so an
// existing row for it is cleared first — a leaked row from a killed run must
// not fail every later run.
func seedDNSFixture(t *testing.T, ctx context.Context, st *store.Store, serverName, domain, endpoint string, proxyRole bool) dnsFixtureIDs {
	t.Helper()
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "org_dns_" + uniq
	prjID := "prj_dns_" + uniq
	envID := "env_dns_" + uniq
	srvID := "srv_dns_" + uniq
	resID := "res_dns_" + uniq
	domID := "dom_dns_" + uniq

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	exec(`DELETE FROM domains WHERE lower(domain) = lower($1)`, domain)
	exec(`INSERT INTO projects (id, org_id, name) VALUES ($1,$2,$3)`, prjID, orgID, "dns-"+uniq)
	exec(`INSERT INTO environments (id, org_id, project_id, name) VALUES ($1,$2,$3,'production')`, envID, orgID, prjID)
	exec(`INSERT INTO servers (id, org_id, name, endpoint, mesh_ip, proxy_role) VALUES ($1,$2,$3,$4,$5,$6)`,
		srvID, orgID, serverName, endpoint, "100.64.0.7", proxyRole)
	exec(`INSERT INTO resources (id, org_id, project_id, environment_id, server_id, name, kind)
	      VALUES ($1,$2,$3,$4,$5,$6,'app')`, resID, orgID, prjID, envID, srvID, "app-"+uniq)
	exec(`INSERT INTO domains (id, org_id, resource_id, domain) VALUES ($1,$2,$3,$4)`, domID, orgID, resID, domain)

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = st.Pool.Exec(cleanupCtx, `DELETE FROM domains WHERE id = $1`, domID)
		_, _ = st.Pool.Exec(cleanupCtx, `DELETE FROM resources WHERE id = $1`, resID)
		_, _ = st.Pool.Exec(cleanupCtx, `DELETE FROM servers WHERE id = $1`, srvID)
		_, _ = st.Pool.Exec(cleanupCtx, `DELETE FROM environments WHERE id = $1`, envID)
		_, _ = st.Pool.Exec(cleanupCtx, `DELETE FROM projects WHERE id = $1`, prjID)
	})

	return dnsFixtureIDs{orgID: orgID, domainID: domID}
}
