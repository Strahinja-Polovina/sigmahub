// Package gitdetect inspects a repository's root files and derives the deploy
// configuration the connect wizard pre-fills: whether it ships a Dockerfile or a
// Compose file, which ports it exposes, which env vars it references, and any
// declared health check. The parsing is deliberately dependency-light (targeted
// line scans, not a full YAML/Dockerfile grammar) because the result is a
// best-effort pre-fill a human confirms in the UI, not an authoritative build.
package gitdetect

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Detected is the derived deploy configuration for a connected repo.
type Detected struct {
	HasDockerfile bool     `json:"hasDockerfile"`
	HasCompose    bool     `json:"hasCompose"`
	DockerfileT   string   `json:"dockerfilePath,omitempty"`
	ComposePath   string   `json:"composePath,omitempty"`
	Ports         []int    `json:"ports"`
	Env           []string `json:"env"`
	// Services is the Compose service graph (empty for a plain Dockerfile app) —
	// the input to a per-service multi-service deploy.
	Services []ComposeService `json:"services,omitempty"`
	// HealthCheck is always populated: a probe detected from the repo, or — when
	// nothing is declared — a default TCP probe on the primary declared port. It
	// is the spec field the P1-9 zero-downtime gate consumes.
	HealthCheck HealthCheck `json:"healthCheck"`
	// Deployable is false when the repo ships neither a Dockerfile nor a Compose
	// file; Reason then carries an actionable message for the UI.
	Deployable bool   `json:"deployable"`
	Reason     string `json:"reason,omitempty"`
	// DefaultBranch is the repo's default branch as reported by the provider
	// (set by the inspector, not by file detection) — the wizard's auto
	// branch-mapping target.
	DefaultBranch string `json:"defaultBranch,omitempty"`
}

// HealthCheck is the resource's readiness probe. Type is "http" when a path was
// detected, otherwise "tcp". Source records where it came from ("dockerfile",
// "compose", or "default" when synthesized as a TCP probe on the primary port).
type HealthCheck struct {
	Type        string `json:"type"`
	Path        string `json:"path,omitempty"`
	Port        int    `json:"port,omitempty"`
	IntervalSec int    `json:"intervalSec"`
	Source      string `json:"source"`
}

// healthAccum collects health-check hints while scanning; finalized against the
// detected ports so a repo with no declared probe still gets a TCP default.
type healthAccum struct {
	found       bool
	source      string
	path        string
	port        int
	intervalSec int
}

// Candidate file names, in precedence order.
var (
	dockerfileNames = []string{"Dockerfile", "dockerfile"}
	composeNames    = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}
	// EnvExampleNames are repo-root env templates whose KEYS pre-fill the
	// wizard's Variables step (values are placeholders and are never read).
	// Presence never affects deployability.
	EnvExampleNames = []string{".env.example", ".env.sample", ".env.template"}
)

// Detect derives the deploy configuration from a repo's root file set, keyed by
// path. Unknown files are ignored. It never errors — an empty/unrecognized repo
// yields a non-Deployable result with an actionable Reason.
func Detect(files map[string][]byte) Detected {
	d := Detected{Ports: []int{}, Env: []string{}}

	var dockerfile []byte
	for _, name := range dockerfileNames {
		if b, ok := files[name]; ok {
			d.HasDockerfile = true
			d.DockerfileT = name
			dockerfile = b
			break
		}
	}
	var compose []byte
	for _, name := range composeNames {
		if b, ok := files[name]; ok {
			d.HasCompose = true
			d.ComposePath = name
			compose = b
			break
		}
	}

	portSet := map[int]bool{}
	envSet := map[string]bool{}
	var hc healthAccum

	if d.HasDockerfile {
		parseDockerfile(dockerfile, portSet, envSet, &hc)
	}
	if d.HasCompose {
		parseCompose(compose, portSet, envSet, &hc)
		d.Services = ParseComposeServices(compose)
	}
	for _, name := range EnvExampleNames {
		if b, ok := files[name]; ok {
			parseEnvExample(b, envSet)
			break
		}
	}

	for p := range portSet {
		d.Ports = append(d.Ports, p)
	}
	sort.Ints(d.Ports)
	for e := range envSet {
		d.Env = append(d.Env, e)
	}
	sort.Strings(d.Env)

	d.HealthCheck = finalizeHealth(hc, d.Ports)

	d.Deployable = d.HasDockerfile || d.HasCompose
	if !d.Deployable {
		d.Reason = "no Dockerfile or Compose file found at the repository root — sigmahub needs one to build and run this app"
	}
	return d
}

// finalizeHealth turns collected hints into a probe. A declared HTTP path wins
// (http probe); a declared-but-command-only check becomes a TCP probe on the
// primary port; nothing declared synthesizes a default TCP probe — matching the
// SIGMA-46 requirement that a health check is always pre-filled.
func finalizeHealth(hc healthAccum, ports []int) HealthCheck {
	primary := 0
	if len(ports) > 0 {
		primary = ports[0]
	}
	out := HealthCheck{IntervalSec: 10}
	if hc.intervalSec > 0 {
		out.IntervalSec = hc.intervalSec
	}
	switch {
	case hc.found && hc.path != "":
		out.Type, out.Path, out.Source = "http", hc.path, hc.source
		out.Port = hc.port
		if out.Port == 0 {
			out.Port = primary
		}
	case hc.found:
		out.Type, out.Port, out.Source = "tcp", primary, hc.source
	default:
		out.Type, out.Port, out.Source = "tcp", primary, "default"
	}
	return out
}

var (
	exposeRe     = regexp.MustCompile(`(?i)^\s*EXPOSE\s+(.+)$`)
	envRe        = regexp.MustCompile(`(?i)^\s*ENV\s+(.+)$`)
	argRe        = regexp.MustCompile(`(?i)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)`)
	healthRe     = regexp.MustCompile(`(?i)^\s*HEALTHCHECK\s`)
	healthNoneRe = regexp.MustCompile(`(?i)^\s*HEALTHCHECK\s+NONE\s*$`)
	portNumRe    = regexp.MustCompile(`(\d{1,5})`)
	envNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	quoteTrim    = "\"'"
	composePad   = regexp.MustCompile(`^\s*`)
	// Host class excludes ':' so an explicit "host:port" surfaces the port in the
	// capture group instead of being swallowed by the host match.
	urlRe        = regexp.MustCompile(`https?://[^/:\s"']+(?::(\d+))?(/[^\s"'\],)]*)?`)
	intervalRe   = regexp.MustCompile(`(?i)--interval[= ](\d+)(ms|s|m)?`)
	cIntervalRe  = regexp.MustCompile(`(?i)^\s*interval:\s*(\d+)(ms|s|m)?`)
	cPublishedRe = regexp.MustCompile(`^published:\s*"?(\d{1,5})`)
)

// httpProbeFromLine extracts an HTTP path (and optional port) from a health-check
// command line that curls/wgets a URL. Returns ok=false when no URL is present.
func httpProbeFromLine(line string) (path string, port int, ok bool) {
	m := urlRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	if m[1] != "" {
		if p, err := strconv.Atoi(m[1]); err == nil {
			port = p
		}
	}
	path = m[2]
	if path == "" {
		path = "/"
	}
	return path, port, true
}

// durationSec parses a compact "30s" / "2m" / "500ms" duration to whole seconds
// (ms floors to 0, which the caller treats as "use the default interval").
func durationSec(n int, unit string) int {
	switch strings.ToLower(unit) {
	case "m":
		return n * 60
	case "ms":
		return 0
	default: // "s" or empty
		return n
	}
}

func parseDockerfile(b []byte, ports map[int]bool, env map[string]bool, hc *healthAccum) {
	for _, line := range joinContinuations(strings.Split(string(b), "\n")) {
		if m := exposeRe.FindStringSubmatch(line); m != nil {
			for _, tok := range portNumRe.FindAllString(m[1], -1) {
				addPort(ports, tok)
			}
			continue
		}
		if healthNoneRe.MatchString(line) {
			continue // HEALTHCHECK NONE explicitly disables the probe.
		}
		if healthRe.MatchString(line) {
			hc.found = true
			hc.source = "dockerfile"
			if p, port, ok := httpProbeFromLine(line); ok {
				hc.path, hc.port = p, port
			}
			if m := intervalRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				hc.intervalSec = durationSec(n, m[2])
			}
			continue
		}
		if m := envRe.FindStringSubmatch(line); m != nil {
			for _, name := range dockerEnvNames(m[1]) {
				env[name] = true
			}
			continue
		}
		if m := argRe.FindStringSubmatch(line); m != nil {
			env[m[1]] = true
		}
	}
}

// dockerEnvNames extracts the variable name(s) from an ENV instruction, which
// comes in two forms distinguished by the FIRST token: `ENV KEY value` (single —
// the value may itself contain '=') and `ENV K1=v1 K2=v2` (multi). Keying off
// the first token (not "does the line contain '='") is what Docker itself does.
func dockerEnvNames(rest string) []string {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil
	}
	fields := strings.Fields(rest)
	if strings.Contains(fields[0], "=") {
		// Multi-form: KEY=val KEY2=val2 …
		var names []string
		for _, tok := range fields {
			if i := strings.Index(tok, "="); i > 0 {
				if name := tok[:i]; envNameRe.MatchString(name) {
					names = append(names, name)
				}
			}
		}
		return names
	}
	// Single-form: the first token is the key; the rest (which may contain '=')
	// is the value.
	if envNameRe.MatchString(fields[0]) {
		return []string{fields[0]}
	}
	return nil
}

// joinContinuations merges Dockerfile backslash line-continuations so a
// multi-line ENV/EXPOSE/HEALTHCHECK is parsed as one logical instruction.
func joinContinuations(lines []string) []string {
	var out []string
	var buf strings.Builder
	cont := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.HasSuffix(strings.TrimRight(line, " \t"), `\`) {
			buf.WriteString(strings.TrimSuffix(strings.TrimRight(line, " \t"), `\`))
			buf.WriteString(" ")
			cont = true
			continue
		}
		if cont {
			buf.WriteString(line)
			out = append(out, buf.String())
			buf.Reset()
			cont = false
		} else {
			out = append(out, line)
		}
	}
	if cont {
		out = append(out, strings.TrimRight(buf.String(), " "))
	}
	return out
}

// parseEnvExample collects the variable names from a dotenv-style template:
// `KEY=…` / `export KEY=…` lines; comments, blanks and malformed lines are
// ignored. Values are placeholders by definition and are never read.
func parseEnvExample(b []byte, env map[string]bool) {
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		if name := strings.TrimSpace(line[:i]); envNameRe.MatchString(name) {
			env[name] = true
		}
	}
}

func addPort(ports map[int]bool, tok string) {
	tok = strings.Trim(tok, quoteTrim)
	// `8080/tcp` or `8080` — take the leading number.
	if i := strings.IndexAny(tok, "/"); i >= 0 {
		tok = tok[:i]
	}
	if n, err := strconv.Atoi(tok); err == nil && n > 0 && n <= 65535 {
		ports[n] = true
	}
}

// parseCompose does a targeted, indentation-aware scan of a Compose file for the
// three fields the wizard pre-fills: published ports, environment keys, and
// whether any service declares a healthcheck. It is not a general YAML parser;
// it recognizes the common `ports:`/`environment:`/`healthcheck:` block shapes.
func parseCompose(b []byte, ports map[int]bool, env map[string]bool, hc *healthAccum) {
	lines := strings.Split(string(b), "\n")
	mode := "" // "ports" | "env" | "health"
	blockIndent := -1
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(composePad.FindString(line))
		trimmed := strings.TrimSpace(line)

		// Leaving the current block once indentation returns to its header level —
		// EXCEPT a sequence item ("- …") at the header's own column, which YAML
		// allows and which still belongs to the block (a sibling key would be
		// "name:" at that column, not a dash).
		if mode != "" && indent <= blockIndent && !strings.HasPrefix(trimmed, "-") {
			mode = ""
		}

		switch {
		case trimmed == "ports:" || strings.HasPrefix(trimmed, "ports:"):
			mode, blockIndent = "ports", indent
			// Inline flow form: `ports: ["8080:80"]`.
			if rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "ports:")); rest != "" {
				composeInlinePorts(rest, ports)
				mode = ""
			}
			continue
		case trimmed == "environment:" || strings.HasPrefix(trimmed, "environment:"):
			mode, blockIndent = "env", indent
			if rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "environment:")); rest != "" {
				composeInlineEnv(rest, env)
				mode = ""
			}
			continue
		case trimmed == "healthcheck:" || strings.HasPrefix(trimmed, "healthcheck:"):
			mode, blockIndent = "health", indent
			hc.found = true
			hc.source = "compose"
			continue
		}

		switch mode {
		case "ports":
			if strings.HasPrefix(trimmed, "-") {
				composePortItem(strings.TrimSpace(trimmed[1:]), ports)
			} else if m := cPublishedRe.FindStringSubmatch(strings.TrimPrefix(trimmed, "- ")); m != nil {
				// Long syntax: `- target: 80 / published: 8080 / protocol: tcp`.
				addPort(ports, m[1])
			}
		case "env":
			composeEnvItem(trimmed, env)
		case "health":
			// Inside the healthcheck block: pull an HTTP path from the test
			// command and the interval, if present.
			if hc.path == "" {
				if p, port, ok := httpProbeFromLine(trimmed); ok {
					hc.path, hc.port = p, port
				}
			}
			if m := cIntervalRe.FindStringSubmatch(trimmed); m != nil {
				n, _ := strconv.Atoi(m[1])
				hc.intervalSec = durationSec(n, m[2])
			}
		}
	}
}

// composePortItem handles a `ports:` list item: "8080:80", "80", "127.0.0.1:8080:80",
// "8080:80/tcp". The published (host-facing) port is the first port in a pair;
// a bare single value is the port itself.
func composePortItem(item string, ports map[int]bool) {
	item = strings.Trim(strings.TrimSpace(item), quoteTrim)
	if item == "" {
		return
	}
	if i := strings.IndexAny(item, "/"); i >= 0 {
		item = item[:i]
	}
	parts := strings.Split(item, ":")
	// "host:container" → host; "ip:host:container" → middle; single → itself.
	switch len(parts) {
	case 1:
		addPort(ports, parts[0])
	case 2:
		addPort(ports, parts[0])
	default:
		addPort(ports, parts[len(parts)-2])
	}
}

func composeInlinePorts(rest string, ports map[int]bool) {
	rest = strings.Trim(rest, "[]")
	for _, item := range strings.Split(rest, ",") {
		composePortItem(item, ports)
	}
}

func composeEnvItem(item string, env map[string]bool) {
	item = strings.TrimSpace(item)
	if strings.HasPrefix(item, "-") {
		item = strings.TrimSpace(item[1:])
	}
	item = strings.Trim(item, quoteTrim)
	// list form "KEY=value" | "KEY"; map form "KEY: value". Split on whichever
	// delimiter appears FIRST, so a value that itself contains '=' (e.g.
	// "KEY: a=b") or ':' (e.g. "KEY=postgres://…") doesn't truncate the key.
	eq := strings.Index(item, "=")
	colon := strings.Index(item, ":")
	name := item
	switch {
	case eq > 0 && (colon < 0 || eq < colon):
		name = item[:eq]
	case colon > 0:
		name = item[:colon]
	}
	name = strings.TrimSpace(name)
	if envNameRe.MatchString(name) {
		env[name] = true
	}
}

func composeInlineEnv(rest string, env map[string]bool) {
	rest = strings.Trim(rest, "[]{}")
	for _, item := range strings.Split(rest, ",") {
		composeEnvItem(item, env)
	}
}
