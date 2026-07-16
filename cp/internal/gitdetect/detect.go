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
	HealthCheck   string   `json:"healthCheck,omitempty"`
	// Deployable is false when the repo ships neither a Dockerfile nor a Compose
	// file; Reason then carries an actionable message for the UI.
	Deployable bool   `json:"deployable"`
	Reason     string `json:"reason,omitempty"`
}

// Candidate file names, in precedence order.
var (
	dockerfileNames = []string{"Dockerfile", "dockerfile"}
	composeNames    = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}
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

	if d.HasDockerfile {
		parseDockerfile(dockerfile, portSet, envSet, &d)
	}
	if d.HasCompose {
		parseCompose(compose, portSet, envSet, &d)
	}

	for p := range portSet {
		d.Ports = append(d.Ports, p)
	}
	sort.Ints(d.Ports)
	for e := range envSet {
		d.Env = append(d.Env, e)
	}
	sort.Strings(d.Env)

	d.Deployable = d.HasDockerfile || d.HasCompose
	if !d.Deployable {
		d.Reason = "no Dockerfile or Compose file found at the repository root — sigmahub needs one to build and run this app"
	}
	return d
}

var (
	exposeRe   = regexp.MustCompile(`(?i)^\s*EXPOSE\s+(.+)$`)
	envRe      = regexp.MustCompile(`(?i)^\s*ENV\s+(.+)$`)
	argRe      = regexp.MustCompile(`(?i)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)`)
	healthRe   = regexp.MustCompile(`(?i)^\s*HEALTHCHECK\s`)
	portNumRe  = regexp.MustCompile(`(\d{1,5})`)
	envNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	quoteTrim  = "\"'"
	composePad = regexp.MustCompile(`^\s*`)
)

func parseDockerfile(b []byte, ports map[int]bool, env map[string]bool, d *Detected) {
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, "\r")
		if m := exposeRe.FindStringSubmatch(line); m != nil {
			for _, tok := range portNumRe.FindAllString(m[1], -1) {
				addPort(ports, tok)
			}
			continue
		}
		if healthRe.MatchString(line) && !strings.Contains(strings.ToUpper(line), "NONE") {
			d.HealthCheck = "docker HEALTHCHECK"
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
// comes in two forms: `ENV KEY value` (single) and `ENV K1=v1 K2=v2` (multi).
func dockerEnvNames(rest string) []string {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil
	}
	if strings.Contains(rest, "=") {
		var names []string
		for _, tok := range strings.Fields(rest) {
			if i := strings.Index(tok, "="); i > 0 {
				name := tok[:i]
				if envNameRe.MatchString(name) {
					names = append(names, name)
				}
			}
		}
		return names
	}
	// `ENV KEY the rest is the value` — first field is the name.
	name := strings.Fields(rest)[0]
	if envNameRe.MatchString(name) {
		return []string{name}
	}
	return nil
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
func parseCompose(b []byte, ports map[int]bool, env map[string]bool, d *Detected) {
	lines := strings.Split(string(b), "\n")
	mode := "" // "ports" | "env"
	blockIndent := -1
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(composePad.FindString(line))
		trimmed := strings.TrimSpace(line)

		// Leaving the current block once indentation returns to its header level.
		if mode != "" && indent <= blockIndent {
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
			d.HealthCheck = "compose healthcheck"
			continue
		}

		switch mode {
		case "ports":
			if strings.HasPrefix(trimmed, "-") {
				composePortItem(strings.TrimSpace(trimmed[1:]), ports)
			}
		case "env":
			composeEnvItem(trimmed, env)
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
	// list form "- KEY=value" or "- KEY"; map form "KEY: value".
	var name string
	if i := strings.Index(item, "="); i > 0 {
		name = item[:i]
	} else if i := strings.Index(item, ":"); i > 0 {
		name = item[:i]
	} else {
		name = item
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
