package gitdetect

import (
	"reflect"
	"strings"
	"testing"
)

func TestDetectDockerfile(t *testing.T) {
	df := []byte(`FROM node:20
WORKDIR /app
ARG BUILD_MODE
ENV NODE_ENV=production PORT=3000
ENV LEGACY_SINGLE the value here
EXPOSE 3000
EXPOSE 8080/tcp
HEALTHCHECK --interval=30s CMD curl -f http://localhost:3000/health || exit 1
CMD ["node","server.js"]`)

	d := Detect(map[string][]byte{"Dockerfile": df})
	if !d.HasDockerfile || d.HasCompose {
		t.Fatalf("HasDockerfile=%v HasCompose=%v", d.HasDockerfile, d.HasCompose)
	}
	if !d.Deployable {
		t.Fatal("Dockerfile repo must be deployable")
	}
	if want := []int{3000, 8080}; !reflect.DeepEqual(d.Ports, want) {
		t.Errorf("ports = %v, want %v", d.Ports, want)
	}
	wantEnv := []string{"BUILD_MODE", "LEGACY_SINGLE", "NODE_ENV", "PORT"}
	if !reflect.DeepEqual(d.Env, wantEnv) {
		t.Errorf("env = %v, want %v", d.Env, wantEnv)
	}
	hc := d.HealthCheck
	if hc.Type != "http" || hc.Path != "/health" || hc.Port != 3000 || hc.Source != "dockerfile" {
		t.Errorf("health check = %+v, want http /health:3000 from dockerfile", hc)
	}
	if hc.IntervalSec != 30 {
		t.Errorf("interval = %d, want 30 (from --interval=30s)", hc.IntervalSec)
	}
}

func TestDetectHealthcheckNone(t *testing.T) {
	// HEALTHCHECK NONE declares no probe → a default TCP probe on the primary
	// declared port is synthesized (SIGMA-46: always pre-filled).
	d := Detect(map[string][]byte{"Dockerfile": []byte("FROM x\nEXPOSE 8080\nHEALTHCHECK NONE\n")})
	hc := d.HealthCheck
	if hc.Type != "tcp" || hc.Source != "default" || hc.Port != 8080 {
		t.Errorf("HEALTHCHECK NONE → health check = %+v, want default tcp:8080", hc)
	}
}

func TestDetectDefaultTCPProbe(t *testing.T) {
	// No health check declared at all → default TCP probe on the primary port.
	d := Detect(map[string][]byte{"Dockerfile": []byte("FROM x\nEXPOSE 5000\n")})
	if d.HealthCheck.Type != "tcp" || d.HealthCheck.Port != 5000 || d.HealthCheck.Source != "default" {
		t.Errorf("default probe = %+v, want tcp:5000 default", d.HealthCheck)
	}
	if d.HealthCheck.IntervalSec != 10 {
		t.Errorf("default interval = %d, want 10", d.HealthCheck.IntervalSec)
	}
}

func TestDetectCompose(t *testing.T) {
	compose := []byte(`services:
  web:
    build: .
    ports:
      - "8080:80"
      - "127.0.0.1:5432:5432"
      - "9090"
    environment:
      - API_KEY=secret
      - DEBUG
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost"]
  worker:
    image: worker
    environment:
      QUEUE_URL: amqp://x
`)
	d := Detect(map[string][]byte{"docker-compose.yml": compose})
	if !d.HasCompose || d.HasDockerfile {
		t.Fatalf("HasCompose=%v HasDockerfile=%v", d.HasCompose, d.HasDockerfile)
	}
	if !d.Deployable {
		t.Fatal("compose repo must be deployable")
	}
	// Published host ports: 8080, 5432 (middle of ip:host:container), 9090.
	if want := []int{5432, 8080, 9090}; !reflect.DeepEqual(d.Ports, want) {
		t.Errorf("ports = %v, want %v", d.Ports, want)
	}
	wantEnv := []string{"API_KEY", "DEBUG", "QUEUE_URL"}
	if !reflect.DeepEqual(d.Env, wantEnv) {
		t.Errorf("env = %v, want %v", d.Env, wantEnv)
	}
	if d.HealthCheck.Source != "compose" {
		t.Errorf("compose healthcheck should be detected, got %+v", d.HealthCheck)
	}
	// The healthcheck test curls http://localhost → an HTTP probe on path "/".
	if d.HealthCheck.Type != "http" || d.HealthCheck.Path != "/" {
		t.Errorf("compose health probe = %+v, want http /", d.HealthCheck)
	}
	// The service graph is surfaced for a multi-service deploy: web builds from
	// source (fixed host port → recreate), worker runs a prebuilt image (stateless
	// → blue-green).
	if len(d.Services) != 2 {
		t.Fatalf("expected 2 services, got %d: %+v", len(d.Services), d.Services)
	}
	web := svcByName(d.Services, "web")
	if web == nil || web.Build != "." || web.Rollout != RolloutBlueGreen {
		t.Errorf("web service = %+v", web)
	}
	worker := svcByName(d.Services, "worker")
	if worker == nil || worker.Image != "worker" || worker.Rollout != RolloutBlueGreen {
		t.Errorf("worker service = %+v", worker)
	}
}

func TestDetectInlineCompose(t *testing.T) {
	compose := []byte(`services:
  app:
    ports: ["3000:3000", "443:8443"]
`)
	d := Detect(map[string][]byte{"compose.yaml": compose})
	if want := []int{443, 3000}; !reflect.DeepEqual(d.Ports, want) {
		t.Errorf("inline ports = %v, want %v", d.Ports, want)
	}
}

func TestDetectUndeployable(t *testing.T) {
	d := Detect(map[string][]byte{"README.md": []byte("# hi"), "main.go": []byte("package main")})
	if d.Deployable {
		t.Fatal("a repo with neither Dockerfile nor compose must be undeployable")
	}
	if d.Reason == "" {
		t.Error("undeployable result must carry an actionable reason")
	}
}

func TestDetectEmpty(t *testing.T) {
	d := Detect(map[string][]byte{})
	if d.Deployable || d.Reason == "" {
		t.Errorf("empty repo must be undeployable with a reason; got %+v", d)
	}
	// Ports/Env must be non-nil empty slices (clean JSON, not null).
	if d.Ports == nil || d.Env == nil {
		t.Error("Ports/Env must be non-nil slices")
	}
}

// TestDetectComposeSameIndentList covers the common YAML style where a sequence
// dash sits at the same column as its key — items must not be dropped.
func TestDetectComposeSameIndentList(t *testing.T) {
	compose := []byte("services:\n  web:\n    image: nginx\n    ports:\n    - \"8080:80\"\n    environment:\n    - API_KEY=secret\n")
	d := Detect(map[string][]byte{"compose.yaml": compose})
	if len(d.Ports) != 1 || d.Ports[0] != 8080 {
		t.Errorf("same-indent ports = %v, want [8080]", d.Ports)
	}
	if len(d.Env) != 1 || d.Env[0] != "API_KEY" {
		t.Errorf("same-indent env = %v, want [API_KEY]", d.Env)
	}
}

func TestDetectDockerfileEnvValueWithEquals(t *testing.T) {
	// Single-form ENV whose value contains '=' must keep the real key.
	d := Detect(map[string][]byte{"Dockerfile": []byte(
		"FROM x\nENV GREETING hello=world\nENV DATABASE_URL postgres://u:p@h/db?sslmode=require\n")})
	envs := map[string]bool{}
	for _, e := range d.Env {
		envs[e] = true
	}
	if !envs["GREETING"] || !envs["DATABASE_URL"] {
		t.Errorf("env = %v, want GREETING and DATABASE_URL", d.Env)
	}
	if envs["hello"] {
		t.Errorf("must not emit bogus key 'hello' from the value: %v", d.Env)
	}
}

func TestDetectDockerfileEnvContinuation(t *testing.T) {
	d := Detect(map[string][]byte{"Dockerfile": []byte("FROM x\nENV FOO=1 \\\n    BAR=2 \\\n    BAZ=3\n")})
	for _, want := range []string{"FOO", "BAR", "BAZ"} {
		found := false
		for _, e := range d.Env {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("continuation env missing %q; got %v", want, d.Env)
		}
	}
}

func TestDetectComposeMapEnvValueWithEquals(t *testing.T) {
	compose := []byte("services:\n  a:\n    environment:\n      APP_MODE: cluster=on\n      DATABASE_URL: postgres://u:p@h/db?x=1\n")
	d := Detect(map[string][]byte{"compose.yml": compose})
	envs := map[string]bool{}
	for _, e := range d.Env {
		envs[e] = true
	}
	if !envs["APP_MODE"] || !envs["DATABASE_URL"] {
		t.Errorf("map-form env = %v, want APP_MODE and DATABASE_URL", d.Env)
	}
}

func TestDetectHealthURLPort(t *testing.T) {
	d := Detect(map[string][]byte{"Dockerfile": []byte(
		"FROM x\nEXPOSE 3000\nHEALTHCHECK CMD curl -f http://localhost:8080/health\n")})
	if d.HealthCheck.Type != "http" || d.HealthCheck.Path != "/health" || d.HealthCheck.Port != 8080 {
		t.Errorf("health probe = %+v, want http /health:8080 (explicit URL port)", d.HealthCheck)
	}
}

func TestDetectHealthNoneSubstringNotDisabled(t *testing.T) {
	// A probe path containing 'none' must not be mistaken for HEALTHCHECK NONE.
	d := Detect(map[string][]byte{"Dockerfile": []byte(
		"FROM x\nEXPOSE 8080\nHEALTHCHECK CMD curl -f http://localhost:8080/nonexistent-guard || exit 1\n")})
	if d.HealthCheck.Source == "default" {
		t.Errorf("declared probe with 'none' substring was wrongly disabled: %+v", d.HealthCheck)
	}
	if d.HealthCheck.Type != "http" || d.HealthCheck.Path != "/nonexistent-guard" {
		t.Errorf("health probe = %+v, want http /nonexistent-guard", d.HealthCheck)
	}
}

func TestDetectComposeLongFormPorts(t *testing.T) {
	compose := []byte("services:\n  web:\n    ports:\n      - target: 80\n        published: 8080\n        protocol: tcp\n")
	d := Detect(map[string][]byte{"compose.yaml": compose})
	found := false
	for _, p := range d.Ports {
		if p == 8080 {
			found = true
		}
	}
	if !found {
		t.Errorf("long-form ports = %v, want to include published 8080", d.Ports)
	}
}

func TestDetectBothPrecedence(t *testing.T) {
	// A repo with both: Dockerfile chosen for name, and ports merge across both.
	d := Detect(map[string][]byte{
		"Dockerfile":         []byte("FROM x\nEXPOSE 3000\n"),
		"docker-compose.yml": []byte("services:\n  a:\n    ports:\n      - \"80:80\"\n"),
	})
	if !d.HasDockerfile || !d.HasCompose {
		t.Fatal("both should be flagged present")
	}
	if want := []int{80, 3000}; !reflect.DeepEqual(d.Ports, want) {
		t.Errorf("merged ports = %v, want %v", d.Ports, want)
	}
}

// ── Detection below the repository root (monorepos) ──────────────────────────

// A repository whose root describes no build is not an undeployable repository.
// It is the most common shape there is — a workspace with the app one or two
// directories down — and the old root-only search called every one of them
// "not deployable", which is both false and the least useful thing we could
// have said.
func TestDetectBelowRoot(t *testing.T) {
	cases := []struct {
		name        string
		files       map[string][]byte
		wantMethod  string
		wantSubdir  string
		wantDocker  string
		wantCompose string
	}{
		{
			name: "root wins outright when the root describes a build",
			files: map[string][]byte{
				"Dockerfile":          []byte("FROM x\nEXPOSE 3000\n"),
				"apps/api/Dockerfile": []byte("FROM y\nEXPOSE 9999\n"),
			},
			wantMethod: BuildDockerfile,
			wantSubdir: "",
			wantDocker: "Dockerfile",
		},
		{
			name: "single nested app",
			files: map[string][]byte{
				"README.md":           []byte("# hi"),
				"apps/api/Dockerfile": []byte("FROM x\nEXPOSE 8080\n"),
			},
			wantMethod: BuildDockerfile,
			wantSubdir: "apps/api",
			wantDocker: "Dockerfile",
		},
		{
			name: "shallower beats deeper",
			files: map[string][]byte{
				"src/Dockerfile":            []byte("FROM x\nEXPOSE 1111\n"),
				"packages/api/Dockerfile":   []byte("FROM y\nEXPOSE 2222\n"),
				"packages/api/x/Dockerfile": []byte("FROM z\nEXPOSE 3333\n"),
			},
			wantMethod: BuildDockerfile,
			wantSubdir: "src",
			wantDocker: "Dockerfile",
		},
		{
			// Two candidates at the same depth: the service-ish name is picked so
			// the answer is the same on every run. Map iteration order is not an
			// answer to "which app did you mean".
			name: "same depth resolves by name, deterministically",
			files: map[string][]byte{
				"zzz/Dockerfile": []byte("FROM x\nEXPOSE 1111\n"),
				"api/Dockerfile": []byte("FROM y\nEXPOSE 2222\n"),
			},
			wantMethod: BuildDockerfile,
			wantSubdir: "api",
			wantDocker: "Dockerfile",
		},
		{
			name: "nested compose",
			files: map[string][]byte{
				"deploy/docker-compose.yml": []byte("services:\n  web:\n    build: .\n    ports:\n      - \"8080:80\"\n"),
			},
			wantMethod:  BuildCompose,
			wantSubdir:  "deploy",
			wantCompose: "deploy/docker-compose.yml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Detect(tc.files)
			if !d.Deployable {
				t.Fatalf("must be deployable: %+v", d)
			}
			if d.BuildMethod != tc.wantMethod {
				t.Errorf("build method = %q, want %q", d.BuildMethod, tc.wantMethod)
			}
			if d.ContextSubdir != tc.wantSubdir {
				t.Errorf("context subdir = %q, want %q", d.ContextSubdir, tc.wantSubdir)
			}
			if d.DockerfileT != tc.wantDocker {
				t.Errorf("dockerfile = %q, want %q", d.DockerfileT, tc.wantDocker)
			}
			if d.ComposePath != tc.wantCompose {
				t.Errorf("compose path = %q, want %q", d.ComposePath, tc.wantCompose)
			}
		})
	}
}

// A Dockerfile found in a subdirectory has to be addressed as (context,
// dockerfile) — the pair the agent's image.build op takes. Reporting the
// repo-relative path in BOTH fields would send the build looking for
// apps/api/apps/api/Dockerfile.
func TestDetectSubdirDockerfileIsRelativeToItsContext(t *testing.T) {
	d := Detect(map[string][]byte{
		"apps/api/dockerfile": []byte("FROM x\nEXPOSE 8080\nENV TOKEN=x\n"),
	})
	if d.ContextSubdir != "apps/api" || d.DockerfileT != "dockerfile" {
		t.Fatalf("context/dockerfile = %q/%q, want apps/api/dockerfile split across the two", d.ContextSubdir, d.DockerfileT)
	}
}

// Compose service build contexts are relative to the COMPOSE FILE. The clone is
// the repo root, so a compose file in a subdirectory has to have that directory
// folded into every service before the graph leaves detection — otherwise every
// service builds the wrong tree, or nothing.
func TestDetectNestedComposeRebasesServiceContexts(t *testing.T) {
	d := Detect(map[string][]byte{
		"deploy/compose.yaml": []byte("services:\n  web:\n    build: .\n  worker:\n    build: ./worker\n  cache:\n    image: redis:7.4\n"),
	})
	byName := map[string]ComposeService{}
	for _, s := range d.Services {
		byName[s.Name] = s
	}
	if got := byName["web"].Build; got != "deploy" {
		t.Errorf("web build context = %q, want deploy", got)
	}
	if got := byName["worker"].Build; got != "deploy/worker" {
		t.Errorf("worker build context = %q, want deploy/worker", got)
	}
	if byName["cache"].Build != "" || byName["cache"].Image != "redis:7.4" {
		t.Errorf("a prebuilt service must not acquire a build context: %+v", byName["cache"])
	}
}

// A root .env.example belongs to a root build; a nested one belongs to the
// nested build it sits beside. Reading the wrong one pre-fills the wizard with
// another service's variables.
func TestDetectEnvTemplateFollowsTheBuildDirectory(t *testing.T) {
	d := Detect(map[string][]byte{
		".env.example":          []byte("ROOT_ONLY=1\n"),
		"apps/api/Dockerfile":   []byte("FROM x\nEXPOSE 8080\n"),
		"apps/api/.env.example": []byte("API_KEY=\nDATABASE_URL=\n"),
	})
	if !reflect.DeepEqual(d.Env, []string{"API_KEY", "DATABASE_URL"}) {
		t.Errorf("env = %v, want the app's own template, not the root's", d.Env)
	}
}

// ── The nixpacks fallback ───────────────────────────────────────────────────

// "Not deployable, go away" was the worst dead end in the product, and it was
// reached by the most ordinary repository there is: one that says how to RUN
// itself and never says how to containerize itself.
func TestDetectNixpacksLanguages(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string][]byte
		wantLang string
		wantPort int
	}{
		{"node", map[string][]byte{"package.json": []byte(`{"name":"a"}`)}, "node", 3000},
		{"go", map[string][]byte{"go.mod": []byte("module a\n")}, "go", 8080},
		{"python via pyproject", map[string][]byte{"pyproject.toml": []byte("[project]\n")}, "python", 8000},
		{"python via requirements", map[string][]byte{"requirements.txt": []byte("flask\n")}, "python", 8000},
		{"ruby", map[string][]byte{"Gemfile": []byte("source 'x'\n")}, "ruby", 3000},
		{"php", map[string][]byte{"composer.json": []byte("{}")}, "php", 8080},
		{"rust", map[string][]byte{"Cargo.toml": []byte("[package]\n")}, "rust", 8080},
		{"java maven", map[string][]byte{"pom.xml": []byte("<project/>")}, "java", 8080},
		{"java gradle", map[string][]byte{"build.gradle.kts": []byte("plugins {}")}, "java", 8080},
		{"elixir", map[string][]byte{"mix.exs": []byte("defmodule A do end")}, "elixir", 4000},
		{"dotnet", map[string][]byte{"global.json": []byte("{}")}, "dotnet", 8080},
		// Deno projects often carry a package.json for editor tooling; the
		// runtime is still Deno, so the ordered catalog has to resolve it that way.
		{"deno beats node", map[string][]byte{"deno.json": []byte("{}"), "package.json": []byte("{}")}, "deno", 8000},
		// The language may also be below the root.
		{"nested", map[string][]byte{"README.md": []byte("#"), "services/api/go.mod": []byte("module a\n")}, "go", 8080},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Detect(tc.files)
			if !d.Deployable {
				t.Fatalf("%s repo must be deployable via nixpacks: %+v", tc.wantLang, d)
			}
			if d.BuildMethod != BuildNixpacks {
				t.Errorf("build method = %q, want %q", d.BuildMethod, BuildNixpacks)
			}
			if d.Language != tc.wantLang {
				t.Errorf("language = %q, want %q", d.Language, tc.wantLang)
			}
			if d.HasDockerfile || d.HasCompose {
				t.Errorf("nixpacks fallback must not claim a Dockerfile/compose: %+v", d)
			}
			// A repo with no Dockerfile has no EXPOSE either, so without the
			// language's conventional port the rollout declares none and its
			// health probe targets nothing.
			if len(d.Ports) != 1 || d.Ports[0] != tc.wantPort {
				t.Errorf("ports = %v, want [%d]", d.Ports, tc.wantPort)
			}
			if d.HealthCheck.Port != tc.wantPort {
				t.Errorf("health probe port = %d, want %d", d.HealthCheck.Port, tc.wantPort)
			}
		})
	}
}

// A Dockerfile always beats a language marker: the repository already said how
// it wants to be built, and guessing over an explicit instruction is not a
// fallback, it is an override.
func TestDetectDockerfileBeatsLanguageMarker(t *testing.T) {
	d := Detect(map[string][]byte{
		"Dockerfile":   []byte("FROM node:20\nEXPOSE 4000\n"),
		"package.json": []byte(`{"name":"a"}`),
	})
	if d.BuildMethod != BuildDockerfile || d.Language != "" {
		t.Errorf("build method/language = %q/%q, want dockerfile with no language guess", d.BuildMethod, d.Language)
	}
	if len(d.Ports) != 1 || d.Ports[0] != 4000 {
		t.Errorf("ports = %v, want the Dockerfile's [4000], not Node's default", d.Ports)
	}
}

// Compose beats a sibling Dockerfile: the compose file describes the WHOLE
// application, including the service that Dockerfile builds. Building only that
// one service is how a four-service repo came to deploy as one container.
func TestDetectComposeBeatsDockerfile(t *testing.T) {
	d := Detect(map[string][]byte{
		"Dockerfile":         []byte("FROM x\nEXPOSE 3000\n"),
		"docker-compose.yml": []byte("services:\n  a:\n    build: .\n  b:\n    image: redis:7.4\n"),
	})
	if d.BuildMethod != BuildCompose {
		t.Errorf("build method = %q, want %q", d.BuildMethod, BuildCompose)
	}
	if len(d.Services) != 2 {
		t.Errorf("compose graph = %+v, want both services", d.Services)
	}
}

// Only a repository nothing can be built from is undeployable, and it has to
// say what to do about it.
func TestDetectTrulyUndeployable(t *testing.T) {
	d := Detect(map[string][]byte{"README.md": []byte("# docs"), "LICENSE": []byte("MIT")})
	if d.Deployable || d.BuildMethod != "" {
		t.Fatalf("a docs repo must be undeployable: %+v", d)
	}
	if !strings.Contains(d.Reason, "Dockerfile") || !strings.Contains(d.Reason, "subdirectory") {
		t.Errorf("reason must name both exits, got %q", d.Reason)
	}
}

// ── What the inspector is told to fetch ─────────────────────────────────────

func TestWantedPaths(t *testing.T) {
	all := []string{
		"README.md",
		"Dockerfile",
		"go.mod",
		"apps/api/Dockerfile",
		"apps/api/.env.example",
		"apps/web/package.json",
		"vendor/a/b/c/package.json", // too deep
		"docs/architecture.md",
	}
	got := WantedPaths(all, 2)
	want := []string{
		"Dockerfile", "go.mod",
		"apps/api/.env.example", "apps/api/Dockerfile", "apps/web/package.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wanted paths = %v, want %v", got, want)
	}
}

// The caller truncates the fetch list, so ordering is load-bearing: a workspace
// with many nested manifests must not push the repository's own root build out
// of the set.
func TestWantedPathsPutsShallowFirst(t *testing.T) {
	all := []string{"a/b/package.json", "a/package.json", "package.json"}
	got := WantedPaths(all, 2)
	if got[0] != "package.json" {
		t.Errorf("shallowest path must come first, got %v", got)
	}
}

func TestCandidatePathsCoversRootAndConventionalDirs(t *testing.T) {
	paths := map[string]bool{}
	for _, p := range CandidatePaths() {
		paths[p] = true
	}
	for _, want := range []string{"Dockerfile", "docker-compose.yml", "go.mod", "api/Dockerfile", "src/package.json"} {
		if !paths[want] {
			t.Errorf("candidate paths missing %q", want)
		}
	}
}
