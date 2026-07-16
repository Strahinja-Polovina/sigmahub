package gitdetect

import (
	"sort"
	"strconv"
	"strings"
)

// ComposeService is one service parsed from a Compose file's `services:` block.
// The parse is dependency-light (indentation-aware line scanning, not a full YAML
// grammar) — a best-effort pre-fill consistent with the rest of gitdetect, which
// a human confirms. The reconciler turns each into its own build + rollout op.
type ComposeService struct {
	Name string `json:"name"`
	// Build is the build context (".", a subdir) when the service builds from
	// source; empty when it runs a prebuilt Image.
	Build      string `json:"build,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Image      string `json:"image,omitempty"`
	// Ports are the container ports the service exposes (target side).
	Ports []int `json:"ports,omitempty"`
	// PublishedPorts are fixed host ports the service binds (host:container). A
	// non-empty set forces the recreate rollout class — a fixed host port can't be
	// held by two generations at once.
	PublishedPorts []int `json:"publishedPorts,omitempty"`
	// NamedVolumes are docker named volumes the service mounts (source is a bare
	// name, not a ./ or / path). A named volume implies exclusive state → recreate.
	NamedVolumes []string `json:"namedVolumes,omitempty"`
	DependsOn    []string `json:"dependsOn,omitempty"`
	// Rollout is the swap class: "blue-green" for stateless services, "recreate"
	// for services holding exclusive resources (named volume or fixed host port) —
	// the documented per-service exception to the zero-downtime guarantee.
	Rollout string `json:"rollout"`
}

// Rollout class constants shared with the reconciler render.
const (
	RolloutBlueGreen = "blue-green"
	RolloutRecreate  = "recreate"
)

// ParseComposeServices extracts the service graph from a Compose file. It scans
// the top-level `services:` block by indentation; each 2nd-level key is a service
// whose build/image/ports/volumes/depends_on are read from its own sub-block.
// Unknown keys are ignored. Services are returned in declaration order.
func ParseComposeServices(b []byte) []ComposeService {
	lines := strings.Split(string(b), "\n")

	var services []ComposeService
	cur := -1           // index into services of the service being filled
	inServices := false // inside the top-level services: block
	svcIndent := -1     // indent of service-name keys (set from the first one)
	block := ""         // sub-list mode: "ports" | "volumes" | "depends_on"
	blockIndent := -1   // indent of the sub-list header
	buildBlock := false // inside a multi-line build: block
	buildBlockIndent := -1

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(composePad.FindString(line))

		// Top-level keys (indent 0) delimit the services block.
		if indent == 0 {
			inServices = strings.HasPrefix(trimmed, "services:")
			cur, block, buildBlock = -1, "", false
			svcIndent = -1
			continue
		}
		if !inServices {
			continue
		}

		// The first indented key under services: fixes the service-name column.
		if svcIndent == -1 {
			svcIndent = indent
		}

		// A key at the service-name column starts a new service.
		if indent == svcIndent && !strings.HasPrefix(trimmed, "-") {
			name := strings.TrimSuffix(strings.SplitN(trimmed, ":", 2)[0], ":")
			name = strings.TrimSpace(name)
			services = append(services, ComposeService{Name: name})
			cur = len(services) - 1
			block, buildBlock = "", false
			continue
		}
		if cur < 0 {
			continue
		}
		svc := &services[cur]

		// Leave a sub-list when indentation returns to (or above) its header,
		// unless this is a sequence item at the header's own column.
		if block != "" && indent <= blockIndent && !strings.HasPrefix(trimmed, "-") {
			block = ""
		}
		if buildBlock && indent <= buildBlockIndent {
			buildBlock = false
		}

		// Service-level keys (indented one level under the service name).
		switch {
		case strings.HasPrefix(trimmed, "image:"):
			svc.Image = unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "image:")))
			continue
		case trimmed == "build:" || strings.HasPrefix(trimmed, "build:"):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "build:"))
			if rest == "" {
				buildBlock, buildBlockIndent = true, indent
				if svc.Build == "" {
					svc.Build = "."
				}
			} else {
				svc.Build = unquote(rest)
			}
			continue
		case trimmed == "ports:" || strings.HasPrefix(trimmed, "ports:"):
			block, blockIndent = "ports", indent
			if rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "ports:")); rest != "" {
				for _, item := range splitInlineList(rest) {
					composeServicePort(item, svc)
				}
				block = ""
			}
			continue
		case trimmed == "volumes:" || strings.HasPrefix(trimmed, "volumes:"):
			block, blockIndent = "volumes", indent
			continue
		case trimmed == "depends_on:" || strings.HasPrefix(trimmed, "depends_on:"):
			block, blockIndent = "depends_on", indent
			if rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "depends_on:")); rest != "" {
				for _, item := range splitInlineList(rest) {
					if d := strings.TrimSpace(item); d != "" {
						svc.DependsOn = append(svc.DependsOn, d)
					}
				}
				block = ""
			}
			continue
		}

		if buildBlock {
			if strings.HasPrefix(trimmed, "context:") {
				svc.Build = unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "context:")))
			} else if strings.HasPrefix(trimmed, "dockerfile:") {
				svc.Dockerfile = unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "dockerfile:")))
			}
			continue
		}

		switch block {
		case "ports":
			// Strip a leading sequence dash so the long form `- target: 8000`
			// (dash + key on one line) and the short form `- "8080:80"` share a path.
			item := trimmed
			if strings.HasPrefix(item, "-") {
				item = strings.TrimSpace(item[1:])
			}
			switch {
			case strings.HasPrefix(item, "target:"):
				if p, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(item, "target:"))); err == nil {
					svc.Ports = appendUnique(svc.Ports, p)
				}
			case strings.HasPrefix(item, "published:"):
				if m := cPublishedRe.FindStringSubmatch(item); m != nil {
					if p, err := strconv.Atoi(m[1]); err == nil {
						svc.PublishedPorts = appendUnique(svc.PublishedPorts, p)
					}
				}
			case strings.HasPrefix(item, "protocol:"), strings.HasPrefix(item, "mode:"), strings.HasPrefix(item, "host_ip:"), strings.HasPrefix(item, "name:"):
				// long-form scalar keys with no port value — ignore
			default:
				composeServicePort(item, svc)
			}
		case "volumes":
			if strings.HasPrefix(trimmed, "-") {
				composeServiceVolume(strings.TrimSpace(trimmed[1:]), svc)
			} else if strings.HasPrefix(trimmed, "source:") {
				src := unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "source:")))
				if isNamedVolume(src) {
					svc.NamedVolumes = appendUniqueStr(svc.NamedVolumes, src)
				}
			}
		case "depends_on":
			if strings.HasPrefix(trimmed, "-") {
				if d := strings.TrimSpace(unquote(strings.TrimSpace(trimmed[1:]))); d != "" {
					svc.DependsOn = append(svc.DependsOn, d)
				}
			} else if k := strings.TrimSuffix(strings.SplitN(trimmed, ":", 2)[0], ":"); k != "" && indent > blockIndent {
				// Map form: `depends_on:\n  db:\n    condition: ...`.
				svc.DependsOn = append(svc.DependsOn, strings.TrimSpace(k))
			}
		}
	}

	for i := range services {
		s := &services[i]
		sort.Ints(s.Ports)
		sort.Ints(s.PublishedPorts)
		if len(s.NamedVolumes) > 0 || len(s.PublishedPorts) > 0 {
			s.Rollout = RolloutRecreate
		} else {
			s.Rollout = RolloutBlueGreen
		}
	}
	return services
}

// composeServicePort records a service's container port and, when a host port is
// bound (host:container), the published host port.
func composeServicePort(item string, svc *ComposeService) {
	item = unquote(strings.TrimSpace(item))
	if item == "" {
		return
	}
	// Strip a leading host IP (e.g. 127.0.0.1:8080:80).
	parts := strings.Split(item, ":")
	// Drop a /proto suffix on the last field.
	last := parts[len(parts)-1]
	if i := strings.Index(last, "/"); i >= 0 {
		parts[len(parts)-1] = last[:i]
	}
	switch len(parts) {
	case 1: // "80" — container port only, no host binding
		if p, err := strconv.Atoi(parts[0]); err == nil {
			svc.Ports = appendUnique(svc.Ports, p)
		}
	default: // "host:container" (optionally with a leading IP)
		container := parts[len(parts)-1]
		host := parts[len(parts)-2]
		if p, err := strconv.Atoi(container); err == nil {
			svc.Ports = appendUnique(svc.Ports, p)
		}
		if p, err := strconv.Atoi(host); err == nil {
			svc.PublishedPorts = appendUnique(svc.PublishedPorts, p)
		}
	}
}

// composeServiceVolume records a named-volume mount (source is a bare name, not a
// bind path). Bind mounts (./ or /) don't hold exclusive docker state.
func composeServiceVolume(item string, svc *ComposeService) {
	item = unquote(strings.TrimSpace(item))
	if item == "" {
		return
	}
	src := item
	if i := strings.Index(item, ":"); i >= 0 {
		src = item[:i]
	}
	if isNamedVolume(src) {
		svc.NamedVolumes = appendUniqueStr(svc.NamedVolumes, src)
	}
}

// isNamedVolume is true when a volume source is a docker named volume rather than
// a host bind path (which starts with ./, ../, /, or ~).
func isNamedVolume(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" {
		return false
	}
	return !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, ".") && !strings.HasPrefix(src, "~")
}

// splitInlineList splits a YAML inline flow list `[a, "b:c"]` into items.
func splitInlineList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func unquote(s string) string {
	return strings.Trim(strings.TrimSpace(s), quoteTrim)
}

func appendUnique(xs []int, x int) []int {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}

func appendUniqueStr(xs []string, x string) []string {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}
