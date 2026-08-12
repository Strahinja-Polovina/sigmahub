package store

// SIGMA-351: the hostname SigmaHub gives a resource before the customer has any
// DNS of their own.
//
// Routing used to be built solely from customer-attached custom domains, so an
// app that built, deployed and passed its health gate had no address anywhere on
// the internet. The only instruction the product could offer was "buy a domain,
// point it here, come back" — on the first thing a new user tries.
//
// The split between the two halves below is deliberate and is the whole design:
//
//   - PublicLabel is minted once at create and STORED, so it survives a rename.
//     A customer renaming their app must not silently break links they shared.
//   - PublicHost is resolved at RENDER time and never stored, because the suffix
//     depends on deployment config (CP_APPS_DOMAIN) and, in the fallback, on the
//     host's current public address. Both can change under a resource that has
//     not moved.

import (
	"net"
	"regexp"
	"strings"
)

// nonLabelChars is everything that cannot appear in a DNS label.
var nonLabelChars = regexp.MustCompile(`[^a-z0-9]+`)

// publicLabelStemMax caps the human-readable stem so the whole label — stem, a
// hyphen and an 8-character id suffix — stays inside the 63-byte limit on a
// single DNS label with room to spare.
const publicLabelStemMax = 40

// PublicLabel builds a resource's routable label: a dns-shaped stem from its
// name, plus the tail of its id so two resources called "api" in different
// projects cannot collide.
//
// kind is the fallback stem for a name that reduces to nothing — all
// punctuation, or written in a script with no ASCII letters. A label is always
// dns-shaped, because it is going into a hostname either way and "" would
// produce a URL that cannot resolve.
//
// This mirrors the backfill in migration 0065; both must keep agreeing, which is
// why the migration says so in its own comment.
func PublicLabel(name, kind, resourceID string) string {
	stem := strings.Trim(nonLabelChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if stem == "" {
		stem = strings.Trim(nonLabelChars.ReplaceAllString(strings.ToLower(kind), "-"), "-")
	}
	if stem == "" {
		stem = "app"
	}
	if len(stem) > publicLabelStemMax {
		stem = strings.Trim(stem[:publicLabelStemMax], "-")
	}
	suffix := resourceID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return stem + "-" + suffix
}

// PublicHost resolves a label into the hostname Traefik should route, or "" when
// this deployment can offer none.
//
// appsDomain (CP_APPS_DOMAIN) wins when set: the operator has pointed a wildcard
// at their proxy servers, so `<label>.<appsDomain>` routes and certificates issue
// through the ordinary resolver.
//
// Without it, fall back to sslip.io, which resolves any `…-a-b-c-d.sslip.io` to
// a.b.c.d without anyone configuring anything. That is what makes a fresh
// self-hosted install reachable on its FIRST deploy rather than after a DNS
// purchase — the difference between a product that works out of the box and one
// that appears broken.
//
// serverEndpoint is the host's public address as the connect wizard recorded it
// ("ip" or "ip:port"). An endpoint that is absent, a hostname rather than a
// literal, or a private address yields "": a machine behind NAT has no address
// the internet can reach, and inventing one would put a URL on screen that
// silently never resolves — the exact failure this whole change exists to end.
func PublicHost(label, appsDomain, serverEndpoint string) string {
	if label == "" {
		return ""
	}
	if d := strings.Trim(strings.TrimSpace(appsDomain), "."); d != "" {
		return label + "." + d
	}
	ip := publicIPv4(serverEndpoint)
	if ip == "" {
		return ""
	}
	return label + "-" + strings.ReplaceAll(ip, ".", "-") + ".sslip.io"
}

// publicIPv4 extracts a routable IPv4 literal from a recorded endpoint.
//
// IPv4 only, deliberately: the sslip.io dashed form this feeds is an IPv4
// convention, and a v6 host is better served by CP_APPS_DOMAIN than by a URL
// shape that does not exist.
func publicIPv4(endpoint string) string {
	host := strings.TrimSpace(endpoint)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	// Reachable from nowhere the customer will click a link — RFC1918, loopback,
	// link-local, CGNAT (100.64/10, common behind carrier NAT / Starlink) and the
	// reserved test ranges. Reuse the alert-egress predicate so the two never
	// drift, and a NAT'd host never gets handed an sslip.io URL that silently
	// fails to resolve (SIGMA-364).
	if checkPublicIP(host, v4) != nil {
		return ""
	}
	return v4.String()
}
