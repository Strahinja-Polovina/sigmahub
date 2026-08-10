package store

// DNS setup for custom domains.
//
// Attaching a domain is only half the job: nothing routes until the customer
// creates the record at their registrar, and until now the product never said
// which record, pointing where. This derives the exact records from the server
// that will actually serve the domain, and verifies them, so "why isn't my
// domain working" has an answer inside the dashboard.

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DNSRecord is one record the customer must create.
type DNSRecord struct {
	Type  string `json:"type"`  // A | AAAA | CNAME
	Name  string `json:"name"`  // the host part, or "@" for the apex
	Value string `json:"value"` // the address or target
	TTL   int    `json:"ttl"`
}

// DNSSetup is everything the dashboard needs to explain and check a domain.
type DNSSetup struct {
	Domain string `json:"domain"`
	// Apex is true for example.com, false for app.example.com. It matters:
	// a CNAME is illegal at the apex, so an apex domain must use an A record.
	Apex     bool        `json:"apex"`
	Records  []DNSRecord `json:"records"`
	Verified bool        `json:"verified"`
	// Observed is what DNS currently answers, so a mismatch shows the actual
	// wrong value instead of a bare "not verified".
	Observed []string `json:"observed,omitempty"`
	// Reason explains whatever still stands between this domain and a working
	// HTTPS endpoint — including when DNS itself is already correct (SIGMA-299).
	Reason string `json:"reason,omitempty"`
	// ProxyRole is whether the serving server carries the proxy/edge role. It is
	// a hard precondition for issuance, not a detail: the reconciler renders
	// Traefik (and therefore the ACME client and the HTTP-01 responder) only onto
	// proxy-role servers, so with this false the certificate can never issue no
	// matter how correct the record is. The dashboard needs it as its own field
	// so a verified-but-unservable domain can render as a warning instead of a
	// green tick with a caption nobody reads.
	ProxyRole bool `json:"proxyRole"`
	// CertStatus mirrors the domain's certificate state, since issuance is what
	// the DNS record actually unblocks.
	CertStatus string `json:"certStatus"`
	CheckedAt  string `json:"checkedAt"`
}

// defaultDNSTTL is what we suggest: low enough that a correction propagates in
// minutes, high enough not to hammer the customer's DNS provider.
const defaultDNSTTL = 300

// dnsLookupTimeout bounds the verification probe so a slow resolver can't hang
// a dashboard request.
const dnsLookupTimeout = 5 * time.Second

// DNSSetupForDomain derives the required records and checks whether they are
// live. The target address is the PUBLIC endpoint of the server that serves the
// domain — never the mesh address, which resolves to nothing from the internet
// and is the single most likely thing to be pasted in by mistake.
func (s *Store) DNSSetupForDomain(ctx context.Context, orgID, domainID string) (DNSSetup, error) {
	var out DNSSetup
	var endpoint, meshIP, serverName *string
	err := s.Pool.QueryRow(ctx, `
		SELECT d.domain, d.cert_status, sv.name, sv.endpoint, sv.mesh_ip, sv.proxy_role
		  FROM domains d
		  JOIN resources r ON r.id = d.resource_id
		  LEFT JOIN servers sv ON sv.id = r.server_id
		 WHERE d.org_id = $1 AND d.id = $2`, orgID, domainID).
		Scan(&out.Domain, &out.CertStatus, &serverName, &endpoint, &meshIP, &out.ProxyRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return DNSSetup{}, ErrNotFound
	}
	if err != nil {
		return DNSSetup{}, err
	}

	out.Apex = isApexDomain(out.Domain)

	// SIGMA-299: the missing proxy/edge role is a blocking fact about this
	// domain, not a footnote on a DNS mismatch. It used to be mentioned only
	// inside the "resolves, but not to this server" branch, which meant the one
	// case where the role is the ONLY thing left — DNS correct, server not an
	// edge server — came back Verified with an empty Reason and rendered as a
	// green tick. The operator then waits forever on a certificate that nothing
	// on that host will ever request, and the fix lives on a settings page they
	// have just been told they don't need to visit.
	proxyNote := ""
	if !out.ProxyRole {
		proxyNote = serverLabel(serverName) + " isn't marked as a proxy/edge server, so nothing " +
			"terminates TLS for this domain and no certificate can be issued — turn on the " +
			"Proxy / edge role on that server."
	}

	target := publicHost(endpoint)
	if target == "" {
		out.Reason = "This resource's server has no public address yet — it is still enrolling, " +
			"or it is only reachable over the mesh. The record can't be created until it has one."
		out.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		return out, nil
	}

	name := "@"
	if !out.Apex {
		name = strings.TrimSuffix(out.Domain, "."+apexOf(out.Domain))
	}
	recordType := "A"
	if strings.Contains(target, ":") {
		recordType = "AAAA"
	}
	out.Records = []DNSRecord{{Type: recordType, Name: name, Value: target, TTL: defaultDNSTTL}}

	// Verify. A resolution failure is not an error — it is the normal state
	// before the record exists, and saying so is the whole point.
	lookupCtx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	addrs, lookupErr := net.DefaultResolver.LookupHost(lookupCtx, out.Domain)
	out.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	if lookupErr != nil {
		out.Reason = joinReasons("The domain doesn't resolve yet. Create the record below; changes "+
			"can take a few minutes to propagate.", proxyNote)
		return out, nil
	}
	out.Observed = addrs
	for _, a := range addrs {
		if a == target {
			out.Verified = true
			break
		}
	}
	if out.Verified {
		// Verified only says the record is right. proxyNote is empty on a
		// proxy-role server, which is the genuinely-finished state.
		out.Reason = proxyNote
		return out, nil
	}
	out.Reason = joinReasons("The domain resolves, but not to this server. Update the record to "+
		"the value below.", proxyNote)
	return out, nil
}

// joinReasons puts two sentences together without leaving a stray separator
// when the second one doesn't apply.
func joinReasons(first, second string) string {
	if second == "" {
		return first
	}
	if first == "" {
		return second
	}
	return first + " " + second
}

// serverLabel names the server in a sentence an operator can act on. The name
// is what the servers list and the proxy-role switch are labelled with, so
// naming it is the difference between "something is wrong" and "go here".
func serverLabel(name *string) string {
	if name == nil || strings.TrimSpace(*name) == "" {
		return "This resource's server"
	}
	return *name
}

// publicHost extracts the host from a stored "ip:port" endpoint. The port is
// the agent's mesh handshake port and has nothing to do with DNS.
func publicHost(endpoint *string) string {
	if endpoint == nil || *endpoint == "" {
		return ""
	}
	e := *endpoint
	if host, _, err := net.SplitHostPort(e); err == nil {
		return host
	}
	return e
}

// isApexDomain reports whether the name is a registrable apex (example.com)
// rather than a subdomain (app.example.com).
//
// This is a label count, not a public-suffix lookup: it is deliberately
// conservative, and being wrong only means suggesting an A record where a CNAME
// would also have worked — never the reverse, which would be illegal DNS.
func isApexDomain(domain string) bool {
	return strings.Count(strings.Trim(domain, "."), ".") == 1
}

// apexOf returns the last two labels of a domain.
func apexOf(domain string) string {
	parts := strings.Split(strings.Trim(domain, "."), ".")
	if len(parts) < 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
