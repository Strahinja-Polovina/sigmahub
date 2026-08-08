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
	// Reason explains an unverified state in the user's terms.
	Reason string `json:"reason,omitempty"`
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
	var endpoint, meshIP *string
	var proxyRole bool
	err := s.Pool.QueryRow(ctx, `
		SELECT d.domain, d.cert_status, sv.endpoint, sv.mesh_ip, sv.proxy_role
		  FROM domains d
		  JOIN resources r ON r.id = d.resource_id
		  LEFT JOIN servers sv ON sv.id = r.server_id
		 WHERE d.org_id = $1 AND d.id = $2`, orgID, domainID).
		Scan(&out.Domain, &out.CertStatus, &endpoint, &meshIP, &proxyRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return DNSSetup{}, ErrNotFound
	}
	if err != nil {
		return DNSSetup{}, err
	}

	out.Apex = isApexDomain(out.Domain)
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
		out.Reason = "The domain doesn't resolve yet. Create the record below; changes can take " +
			"a few minutes to propagate."
		return out, nil
	}
	out.Observed = addrs
	for _, a := range addrs {
		if a == target {
			out.Verified = true
			break
		}
	}
	if !out.Verified {
		out.Reason = "The domain resolves, but not to this server. Update the record to the value below."
		if !proxyRole {
			out.Reason += " This server also isn't marked as a proxy/edge server, so it won't " +
				"terminate TLS for the domain even once DNS is correct."
		}
	}
	return out, nil
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
