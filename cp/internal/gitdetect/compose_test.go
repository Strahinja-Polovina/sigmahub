package gitdetect

import (
	"reflect"
	"testing"
)

func svcByName(svcs []ComposeService, name string) *ComposeService {
	for i := range svcs {
		if svcs[i].Name == name {
			return &svcs[i]
		}
	}
	return nil
}

func TestParseComposeServices(t *testing.T) {
	compose := []byte(`
services:
  web:
    build: .
    ports:
      - "8080:80"
    depends_on:
      - db
      - cache
    environment:
      - FOO=bar
  api:
    build:
      context: ./api
      dockerfile: Dockerfile.prod
    ports:
      - "80"
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
    ports:
      - "5432:5432"
  cache:
    image: redis:7
    volumes:
      - ./local:/data
volumes:
  pgdata:
`)
	svcs := ParseComposeServices(compose)
	if len(svcs) != 4 {
		t.Fatalf("expected 4 services, got %d: %+v", len(svcs), svcs)
	}

	web := svcByName(svcs, "web")
	if web == nil || web.Build != "." || web.Image != "" {
		t.Fatalf("web build = %+v", web)
	}
	if !reflect.DeepEqual(web.Ports, []int{80}) || !reflect.DeepEqual(web.PublishedPorts, []int{8080}) {
		t.Fatalf("web ports = %v published = %v", web.Ports, web.PublishedPorts)
	}
	if !reflect.DeepEqual(web.DependsOn, []string{"db", "cache"}) {
		t.Fatalf("web depends_on = %v", web.DependsOn)
	}
	// A published host port forces recreate.
	if web.Rollout != RolloutRecreate {
		t.Fatalf("web with a fixed host port must be recreate, got %q", web.Rollout)
	}

	api := svcByName(svcs, "api")
	if api == nil || api.Build != "./api" || api.Dockerfile != "Dockerfile.prod" {
		t.Fatalf("api build block = %+v", api)
	}
	// Container-only port (no host binding) → stateless → blue-green.
	if len(api.PublishedPorts) != 0 || api.Rollout != RolloutBlueGreen {
		t.Fatalf("api should be blue-green with no published ports, got %+v", api)
	}
	if !reflect.DeepEqual(api.Ports, []int{80}) {
		t.Fatalf("api ports = %v", api.Ports)
	}

	db := svcByName(svcs, "db")
	if db == nil || db.Image != "postgres:16" || db.Build != "" {
		t.Fatalf("db = %+v", db)
	}
	if !reflect.DeepEqual(db.NamedVolumes, []string{"pgdata"}) {
		t.Fatalf("db named volumes = %v", db.NamedVolumes)
	}
	// A named volume (and a fixed host port) forces recreate.
	if db.Rollout != RolloutRecreate {
		t.Fatalf("db must be recreate, got %q", db.Rollout)
	}

	cache := svcByName(svcs, "cache")
	if cache == nil || cache.Image != "redis:7" {
		t.Fatalf("cache = %+v", cache)
	}
	// A bind mount (./local) is NOT a named volume → stateless → blue-green.
	if len(cache.NamedVolumes) != 0 || cache.Rollout != RolloutBlueGreen {
		t.Fatalf("cache with a bind mount must be blue-green, got %+v", cache)
	}
}

func TestParseComposeInlineForms(t *testing.T) {
	compose := []byte(`
services:
  app:
    image: nginx:latest
    ports: ["8080:80", "443"]
    depends_on: [redis]
  redis:
    image: redis
`)
	svcs := ParseComposeServices(compose)
	app := svcByName(svcs, "app")
	if app == nil {
		t.Fatal("app missing")
	}
	if !reflect.DeepEqual(app.PublishedPorts, []int{8080}) {
		t.Fatalf("app published = %v", app.PublishedPorts)
	}
	if !reflect.DeepEqual(app.Ports, []int{80, 443}) {
		t.Fatalf("app ports = %v", app.Ports)
	}
	if !reflect.DeepEqual(app.DependsOn, []string{"redis"}) {
		t.Fatalf("app depends_on = %v", app.DependsOn)
	}
	if app.Rollout != RolloutRecreate {
		t.Fatalf("app has a host port → recreate, got %q", app.Rollout)
	}
}

func TestParseComposeLongPortForm(t *testing.T) {
	compose := []byte(`
services:
  svc:
    build: .
    ports:
      - target: 8000
        published: 9000
        protocol: tcp
`)
	svcs := ParseComposeServices(compose)
	svc := svcByName(svcs, "svc")
	if svc == nil {
		t.Fatal("svc missing")
	}
	if !reflect.DeepEqual(svc.Ports, []int{8000}) {
		t.Fatalf("svc target = %v", svc.Ports)
	}
	if !reflect.DeepEqual(svc.PublishedPorts, []int{9000}) {
		t.Fatalf("svc published = %v", svc.PublishedPorts)
	}
}

func TestParseComposeIPPrefixedPort(t *testing.T) {
	compose := []byte(`
services:
  svc:
    build: .
    ports:
      - "127.0.0.1:8080:80/tcp"
`)
	svc := svcByName(ParseComposeServices(compose), "svc")
	if svc == nil {
		t.Fatal("svc missing")
	}
	if !reflect.DeepEqual(svc.Ports, []int{80}) || !reflect.DeepEqual(svc.PublishedPorts, []int{8080}) {
		t.Fatalf("ip-prefixed port = ports %v published %v", svc.Ports, svc.PublishedPorts)
	}
}

func TestParseComposeNoServices(t *testing.T) {
	if got := ParseComposeServices([]byte("version: '3'\n")); len(got) != 0 {
		t.Fatalf("expected no services, got %+v", got)
	}
}
