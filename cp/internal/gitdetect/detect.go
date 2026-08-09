// Package gitdetect inspects a repository's files and derives the deploy
// configuration the connect wizard pre-fills: whether it ships a Dockerfile or a
// Compose file, which ports it exposes, which env vars it references, and any
// declared health check. The parsing is deliberately dependency-light (targeted
// line scans, not a full YAML/Dockerfile grammar) because the result is a
// best-effort pre-fill a human confirms in the UI, not an authoritative build.
//
// Detection is not limited to the repository ROOT. It used to be, and the
// consequence was that a monorepo — a root holding nothing but a README, a
// workspace file and apps/ — was reported as undeployable, which is both false
// and the least actionable thing we could have said about it. The search now
// runs over whatever file set it is handed, prefers the root when the root
// describes a build, and otherwise picks the best-ranked subdirectory that
// does; the chosen directory travels on as ContextSubdir so the build actually
// runs where the Dockerfile is.
package gitdetect

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Detected is the derived deploy configuration for a connected repo.
type Detected struct {
	HasDockerfile bool `json:"hasDockerfile"`
	HasCompose    bool `json:"hasCompose"`
	// DockerfileT is the Dockerfile path RELATIVE TO ContextSubdir, because that
	// is what the agent's image.build op means by "dockerfile" — the two fields
	// are read together and a repo-absolute path here would send the build
	// looking for apps/api/apps/api/Dockerfile.
	DockerfileT string `json:"dockerfilePath,omitempty"`
	// ComposePath is repo-relative, because nothing builds "from" it — it is
	// shown to a human and used to resolve service build contexts.
	ComposePath string   `json:"composePath,omitempty"`
	Ports       []int    `json:"ports"`
	Env         []string `json:"env"`
	// Services is the Compose service graph (empty for a plain Dockerfile app) —
	// the input to a per-service multi-service deploy.
	Services []ComposeService `json:"services,omitempty"`
	// HealthCheck is always populated: a probe detected from the repo, or — when
	// nothing is declared — a default TCP probe on the primary declared port. It
	// is the spec field the P1-9 zero-downtime gate consumes.
	HealthCheck HealthCheck `json:"healthCheck"`
	// Deployable is false only when nothing here can be built AT ALL — not even
	// by the nixpacks fallback. Reason then carries an actionable message.
	Deployable bool   `json:"deployable"`
	Reason     string `json:"reason,omitempty"`
	// DefaultBranch is the repo's default branch as reported by the provider
	// (set by the inspector, not by file detection) — the wizard's auto
	// branch-mapping target.
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// BuildMethod is HOW this repository gets built: BuildDockerfile,
	// BuildCompose, BuildNixpacks, or "" when nothing can. It is a decision,
	// made once, here — the dashboard presents it and lets the user override it,
	// but it never re-derives it, because two implementations of "what is this
	// repo" is how the UI comes to disagree with the thing that builds.
	BuildMethod string `json:"buildMethod"`
	// ContextSubdir is the directory the build runs in, relative to the repo
	// root. Empty means the root, which is the overwhelmingly common case.
	ContextSubdir string `json:"contextSubdir,omitempty"`
	// Language / LanguageLabel are set when BuildMethod is BuildNixpacks: the
	// evidence for the auto-build, so the offer reads "we found a go.mod" and
	// not "trust us".
	Language      string `json:"language,omitempty"`
	LanguageLabel string `json:"languageLabel,omitempty"`
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

// dirIndex groups a path-keyed file set by directory, recording which base
// names each directory holds. Detection is a question about a DIRECTORY ("does
// this place describe a build?"), so asking it directory-first is what lets the
// same rules run at the root and three levels down without a second code path.
type dirIndex map[string]map[string]bool

func indexDirs(files map[string][]byte) dirIndex {
	idx := dirIndex{}
	for p := range files {
		clean := path.Clean(strings.TrimPrefix(p, "./"))
		dir, base := path.Split(clean)
		dir = strings.TrimSuffix(dir, "/")
		if dir == "." {
			dir = ""
		}
		if idx[dir] == nil {
			idx[dir] = map[string]bool{}
		}
		idx[dir][base] = true
	}
	return idx
}

// preferredDirs ranks candidate subdirectories when the ROOT describes no
// build. A monorepo usually holds several buildable directories and we have to
// pick one to pre-fill; picking by name (the service-ish one) beats picking by
// map iteration order, which is what "pick any" would actually mean in Go.
var preferredDirs = []string{
	"app", "api", "backend", "server", "service", "src", "web", "frontend", "cmd", "docker",
}

// dirScore orders candidate directories: shallower always wins, then a
// recognized service-ish leaf name, then lexicographic so the answer is stable
// across runs. Lower is better.
func dirScore(dir string) (int, string) {
	depth := 0
	if dir != "" {
		depth = strings.Count(dir, "/") + 1
	}
	leaf := dir
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		leaf = dir[i+1:]
	}
	rank := len(preferredDirs)
	for i, name := range preferredDirs {
		if leaf == name {
			rank = i
			break
		}
	}
	return depth*1000 + rank, dir
}

// bestDir picks the lowest-scoring directory for which match reports true, or
// ok=false when none does. The root is a candidate like any other and wins by
// construction: its depth is 0.
func bestDir(idx dirIndex, match func(names map[string]bool) bool) (string, bool) {
	best, bestScore, found := "", 0, false
	for dir, names := range idx {
		if !match(names) {
			continue
		}
		score, _ := dirScore(dir)
		if !found || score < bestScore || (score == bestScore && dir < best) {
			best, bestScore, found = dir, score, true
		}
	}
	return best, found
}

// firstPresent returns the first candidate name the directory holds.
func firstPresent(names map[string]bool, candidates []string) (string, bool) {
	for _, c := range candidates {
		if names[c] {
			return c, true
		}
	}
	return "", false
}

func hasBuildFile(names map[string]bool) bool {
	if _, ok := firstPresent(names, dockerfileNames); ok {
		return true
	}
	_, ok := firstPresent(names, composeNames)
	return ok
}

// Detect derives the deploy configuration from a repo's file set, keyed by
// repo-relative path. Unknown files are ignored. It never errors — a repository
// nothing can be built from yields a non-Deployable result with an actionable
// Reason.
func Detect(files map[string][]byte) Detected {
	d := Detected{Ports: []int{}, Env: []string{}}
	idx := indexDirs(files)

	// The build directory: the root when the root says how to build, otherwise
	// the best-ranked subdirectory that does. A root README and an apps/ tree is
	// a repository that can be deployed, and used to be told it could not.
	dir, foundBuild := bestDir(idx, hasBuildFile)
	names := idx[dir]

	var dockerfile, compose []byte
	if foundBuild {
		if name, ok := firstPresent(names, dockerfileNames); ok {
			d.HasDockerfile = true
			// Relative to the context, not to the repo — see the field comment.
			d.DockerfileT = name
			dockerfile = files[path.Join(dir, name)]
		}
		if name, ok := firstPresent(names, composeNames); ok {
			d.HasCompose = true
			d.ComposePath = path.Join(dir, name)
			compose = files[d.ComposePath]
		}
		d.ContextSubdir = dir
	}

	portSet := map[int]bool{}
	envSet := map[string]bool{}
	var hc healthAccum

	if d.HasDockerfile {
		parseDockerfile(dockerfile, portSet, envSet, &hc)
	}
	if d.HasCompose {
		parseCompose(compose, portSet, envSet, &hc)
		// A Compose service's build context is relative to the COMPOSE FILE, so
		// a compose file found in a subdirectory has to have that subdirectory
		// folded into every service before the graph leaves here — the renderer
		// resolves these against the clone root and would otherwise build the
		// wrong tree (or nothing).
		d.Services = rebaseServices(ParseComposeServices(compose), dir)
	}
	// Env templates live beside the build they configure.
	for _, name := range EnvExampleNames {
		if b, ok := files[path.Join(dir, name)]; ok {
			parseEnvExample(b, envSet)
			break
		}
	}

	// Nothing describes a build: fall back to nixpacks if a language is
	// recognizable. "Not deployable, go away" was the worst dead end in the
	// product and it was reached by the most ordinary repository shape there is.
	if !foundBuild {
		langDir, ok := bestDir(idx, func(names map[string]bool) bool {
			_, found := DetectLanguage(names)
			return found
		})
		if ok {
			lang, _ := DetectLanguage(idx[langDir])
			d.BuildMethod = BuildNixpacks
			d.ContextSubdir = langDir
			d.Language = lang.ID
			d.LanguageLabel = lang.Label
			// No Dockerfile means no EXPOSE, so the language's conventional port
			// is the only port there is. Without it the rollout declares none and
			// its probe targets nothing.
			portSet[lang.DefaultPort] = true
			for _, name := range EnvExampleNames {
				if b, ok := files[path.Join(langDir, name)]; ok {
					parseEnvExample(b, envSet)
					break
				}
			}
		}
	} else if d.HasCompose {
		// Compose wins over a sibling Dockerfile: the compose file describes the
		// WHOLE application (including the service that Dockerfile builds), and
		// building only that one service is how a four-service repo came to
		// deploy as one container.
		d.BuildMethod = BuildCompose
	} else {
		d.BuildMethod = BuildDockerfile
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

	d.Deployable = d.BuildMethod != ""
	if !d.Deployable {
		d.Reason = "no Dockerfile, Compose file or recognizable project manifest found — " +
			"add a Dockerfile (the wizard can write you a starter one) or point sigmahub at the subdirectory that holds your app"
	}
	return d
}

// rebaseServices folds the compose file's own directory into each service's
// build context, so every path in the graph is relative to the REPO ROOT — the
// one place the clone exists.
func rebaseServices(services []ComposeService, dir string) []ComposeService {
	if dir == "" || len(services) == 0 {
		return services
	}
	out := make([]ComposeService, len(services))
	copy(out, services)
	for i := range out {
		if out[i].Build == "" {
			continue
		}
		out[i].Build = path.Join(dir, out[i].Build)
	}
	return out
}

// CandidatePaths lists the paths worth fetching when the provider cannot give
// us a file listing (the inspector's fallback). It is the root candidates plus
// the same names under a short list of conventional subdirectories — a poor
// substitute for a real tree listing, but better than root-only.
func CandidatePaths() []string {
	names := buildFileNames()
	out := make([]string, 0, len(names)*(len(preferredDirs)+1))
	out = append(out, names...)
	for _, dir := range preferredDirs {
		for _, name := range names {
			out = append(out, dir+"/"+name)
		}
	}
	return out
}

// WantedPaths filters a repository's full path listing down to the files
// detection actually reads. Depth is capped because a build described eight
// directories deep is not what the wizard should pre-fill, and because the
// listing of a large monorepo is long enough that fetching every match would be
// its own outage.
func WantedPaths(all []string, maxDepth int) []string {
	want := map[string]bool{}
	for _, n := range buildFileNames() {
		want[n] = true
	}
	out := []string{}
	for _, p := range all {
		clean := path.Clean(strings.TrimPrefix(p, "./"))
		if clean == "." || strings.HasPrefix(clean, "../") {
			continue
		}
		depth := strings.Count(clean, "/")
		if depth > maxDepth {
			continue
		}
		if want[path.Base(clean)] {
			out = append(out, clean)
		}
	}
	// Shallowest first, because the caller truncates: a repository with fifty
	// workspace package.json files must not push its own root Dockerfile out of
	// the fetch list. Lexicographic within a depth keeps the result stable.
	sort.Slice(out, func(a, b int) bool {
		da, db := strings.Count(out[a], "/"), strings.Count(out[b], "/")
		if da != db {
			return da < db
		}
		return out[a] < out[b]
	})
	return out
}

// buildFileNames is every base name detection reads: the build files, the env
// templates and the language markers behind the nixpacks fallback.
func buildFileNames() []string {
	out := make([]string, 0, 32)
	out = append(out, dockerfileNames...)
	out = append(out, composeNames...)
	out = append(out, EnvExampleNames...)
	out = append(out, LanguageMarkerNames()...)
	seen := map[string]bool{}
	uniq := out[:0]
	for _, n := range out {
		if seen[n] {
			continue
		}
		seen[n] = true
		uniq = append(uniq, n)
	}
	return uniq
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
