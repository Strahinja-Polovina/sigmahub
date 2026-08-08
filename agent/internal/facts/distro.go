package facts

import "strings"

// ParseOSRelease turns an os-release file body into (normalized id, PRETTY_NAME).
//
// The normalized id is "<ID>-<VERSION_ID>" — the exact vocabulary the control
// plane's catalog already speaks ("ubuntu-24.04", "debian-12"), because until
// SIGMA-201 that string came from a DROPDOWN the operator picked before they
// had ever logged into the machine. Producing anything else here would just
// move the guess into the agent: the whole point is that the reported id can be
// handed to store.DistroSupported unmodified.
//
// A distro with no VERSION_ID (rolling releases: Arch, Debian sid) reports its
// bare id. That is deliberately NOT the same as reporting nothing: "arch" is a
// truthful answer that the registration gate can refuse with a real reason,
// whereas "" means the host could not be asked and leaves the gate nothing to
// say. Only a body with no usable ID at all degrades to empty.
func ParseOSRelease(body string) (id, pretty string) {
	var rawID, version string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		// Blank lines and comments are legal; anything without '=' is not a
		// key/value line. Skipping instead of failing is what keeps a partially
		// corrupt file from costing us the fields that ARE readable.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = unquote(strings.TrimSpace(value))
		switch strings.TrimSpace(key) {
		case "ID":
			rawID = value
		case "VERSION_ID":
			version = value
		case "PRETTY_NAME":
			pretty = value
		}
	}

	rawID = sanitizeDistroToken(rawID)
	version = sanitizeDistroToken(version)
	switch {
	case rawID == "":
		return "", pretty
	case version == "":
		return rawID, pretty
	default:
		return rawID + "-" + version, pretty
	}
}

// unquote strips the shell quoting os-release permits. Values are single- or
// double-quoted or bare; inside double quotes a backslash escapes the next
// character. Unbalanced quotes are left alone rather than half-stripped —
// sanitizeDistroToken drops the stray quote from the id anyway, and PRETTY_NAME
// with a visible quote is a better failure than PRETTY_NAME truncated.
func unquote(v string) string {
	if len(v) < 2 {
		return v
	}
	q := v[0]
	if (q != '"' && q != '\'') || v[len(v)-1] != q {
		return v
	}
	inner := v[1 : len(v)-1]
	if q == '\'' || !strings.ContainsRune(inner, '\\') {
		return inner
	}
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}

// sanitizeDistroToken reduces an os-release token to the character set catalog
// ids use. os-release is an arbitrary file on a machine we do not control and
// the result ends up in a database column, in URLs and in comparisons — so this
// is an allow-list, not an escape. Anything outside it is dropped, and a token
// that had nothing usable in it comes back empty (i.e. "unknown") rather than
// as a mangled id that would compare unequal to everything forever.
func sanitizeDistroToken(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		case r == '-', r == '_', r == ' ':
			// "opensuse leap" and "opensuse_leap" are the same id as
			// "opensuse-leap" as far as the catalog is concerned.
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}
