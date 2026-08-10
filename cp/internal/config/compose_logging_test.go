package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestComposeServicesCapLogSize guards the self-hosting stack against filling
// its own disk with its own logs (SIGMA-254).
//
// Every request the control plane serves produces one structured Info line from
// api.withLogging, and the bulk of that traffic is agent long-polls, heartbeats
// and telemetry pushes — a steady few lines per second for a fleet of any size,
// forever. Docker's json-file driver has no size limit unless one is asked for,
// and the log lives on the same filesystem as the db-data and cp-data volumes.
// So the failure is not "the logs got big": it is Postgres losing the ability to
// write WAL, which presents as a database fault, on a box whose first diagnostic
// (`docker compose logs`) is the thing that ate the disk.
//
// The rule is: a service Docker keeps running forever (restart: unless-stopped)
// must bound its log. A cap belongs in the compose file rather than in the
// host's daemon.json because the compose file is what a self-hoster copies; the
// daemon config is what they never touch.
func TestComposeServicesCapLogSize(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "docker-compose.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	paths := flattenYAMLScalars(src)

	// Long-running = the service is asked to come back up forever. A one-shot
	// or profile-gated helper that exits is not the concern here; a process
	// that Docker restarts indefinitely is, whichever profile it is in.
	var services []string
	for p, v := range paths {
		name, ok := strings.CutPrefix(p, "services.")
		if !ok || strings.Count(name, ".") != 1 || !strings.HasSuffix(name, ".restart") {
			continue
		}
		if v != "unless-stopped" && v != "always" {
			continue
		}
		services = append(services, strings.TrimSuffix(name, ".restart"))
	}
	sort.Strings(services)
	if len(services) == 0 {
		t.Fatal("parsed no long-running services out of the compose file; the parser or the " +
			"file's shape moved and this guard can no longer see what it is guarding")
	}

	for _, svc := range services {
		base := "services." + svc + ".logging"
		// Block style is what the file uses; the flow-style one-liner
		// (`logging: {driver: json-file, options: {max-size: 10m}}`) is
		// accepted too so a future edit in that style is not reported as a
		// missing cap.
		if _, ok := paths[base+".options.max-size"]; ok {
			continue
		}
		if strings.Contains(paths[base], "max-size") {
			continue
		}
		t.Errorf("service %q restarts forever but declares no logging.options.max-size: its "+
			"json-file log grows without bound on the same disk as the db-data and cp-data "+
			"volumes, and the control plane dies of a full disk", svc)
	}
}

// flattenYAMLScalars reads the subset of YAML the compose file is written in and
// returns every scalar keyed by its dotted path ("services.cp.restart"), with
// `*alias` references expanded against their `&anchor`. It is an
// indentation-aware line scan rather than a YAML grammar — the same
// dependency-light approach gitdetect's compose parser takes, and enough for a
// file this repo owns and formats. Sequence entries carry nothing this guard
// reads, so they are skipped.
func flattenYAMLScalars(src []byte) map[string]string {
	out := scanYAMLScalars(src)

	// Anchor expansion. The compose file declares the logging cap once as an
	// x-logging extension field and aliases it into each service, so without
	// this every service would look like it had a bare "*default-logging"
	// scalar and the guard would report a cap that is really there.
	anchors := map[string]string{} // anchor name → path of the node it marks
	for p, v := range out {
		if name, ok := strings.CutPrefix(v, "&"); ok {
			anchors[name] = p
		}
	}
	expanded := map[string]string{} // built separately: never mutate a map mid-range
	for p, v := range out {
		name, ok := strings.CutPrefix(v, "*")
		if !ok {
			continue
		}
		anchored, ok := anchors[name]
		if !ok {
			continue
		}
		for q, qv := range out {
			if suffix, ok := strings.CutPrefix(q, anchored+"."); ok {
				expanded[p+"."+suffix] = qv
			}
		}
	}
	for p, v := range expanded {
		out[p] = v
	}
	return out
}

func scanYAMLScalars(src []byte) map[string]string {
	out := map[string]string{}
	type frame struct {
		indent int
		key    string
	}
	var stack []frame

	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		key, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, frame{indent: indent, key: strings.TrimSpace(key)})

		if val := strings.TrimSpace(rest); val != "" {
			parts := make([]string, 0, len(stack))
			for _, f := range stack {
				parts = append(parts, f.key)
			}
			out[strings.Join(parts, ".")] = strings.Trim(val, `"'`)
		}
	}
	return out
}
