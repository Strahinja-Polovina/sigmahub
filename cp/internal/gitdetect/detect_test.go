package gitdetect

import (
	"reflect"
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
