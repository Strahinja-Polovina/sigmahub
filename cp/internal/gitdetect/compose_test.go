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
	// A published host port is RECORDED but does not force recreate: SigmaHub
	// never binds it (compose ports are exposed, Traefik fronts the service), so
	// the two-generations-cannot-share-a-port argument does not apply. Forcing
	// recreate here told the operator a service would take downtime for a
	// binding the container does not have, and disqualified the web tier from
	// holding its own domain.
	if !reflect.DeepEqual(web.PublishedPorts, []int{8080}) {
		t.Fatalf("web published ports = %v, want the binding recorded", web.PublishedPorts)
	}
	if web.Rollout != RolloutBlueGreen {
		t.Fatalf("a host port must not cost zero-downtime, got %q", web.Rollout)
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
	if app.Rollout != RolloutBlueGreen {
		t.Fatalf("a host port alone must stay blue-green, got %q", app.Rollout)
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

func TestParseComposeLongVolumeForm(t *testing.T) {
	// A stateless service whose only mount is a long-form (map) bind mount must
	// NOT record a phantom named volume ("type"/"target") and must stay
	// blue-green (SIGMA-141). A long-form named volume must still be detected.
	compose := []byte(`
services:
  bindonly:
    build: .
    volumes:
      - type: bind
        source: ./static
        target: /srv/static
        read_only: true
  stateful:
    image: postgres:16
    volumes:
      - type: volume
        source: pgdata
        target: /var/lib/postgresql/data
`)
	svcs := ParseComposeServices(compose)

	bind := svcByName(svcs, "bindonly")
	if bind == nil {
		t.Fatal("bindonly missing")
	}
	if len(bind.NamedVolumes) != 0 {
		t.Fatalf("bind-only long-form mount must record no named volume, got %v", bind.NamedVolumes)
	}
	if bind.Rollout != RolloutBlueGreen {
		t.Fatalf("bind-only service must be blue-green, got %q", bind.Rollout)
	}

	stateful := svcByName(svcs, "stateful")
	if stateful == nil {
		t.Fatal("stateful missing")
	}
	if !reflect.DeepEqual(stateful.NamedVolumes, []string{"pgdata"}) {
		t.Fatalf("stateful long-form named volume = %v", stateful.NamedVolumes)
	}
	if stateful.Rollout != RolloutRecreate {
		t.Fatalf("stateful with a named volume must be recreate, got %q", stateful.Rollout)
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

// The most ordinary compose file there is — a web tier that publishes its port
// — must keep its web service eligible to hold the app's domain.
//
// The renderer picks the web-facing service among services whose rollout is not
// recreate. While a fixed host port forced recreate, a `web` service declaring
// "3000:3000" disqualified itself and the domain landed on whatever service
// came next in the file. Nothing reported it: the app answered on the wrong
// tier and looked like a routing bug in the user's own code.
func TestPublishedPortKeepsAServiceEligibleForTheDomain(t *testing.T) {
	compose := []byte(`
services:
  web:
    build: .
    ports: ["3000:3000"]
    depends_on: [api]
  api:
    build: ./api
    ports: ["8080"]
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
`)
	svcs := ParseComposeServices(compose)
	web, api, db := svcByName(svcs, "web"), svcByName(svcs, "api"), svcByName(svcs, "db")
	if web == nil || api == nil || db == nil {
		t.Fatalf("services = %+v", svcs)
	}
	// web is the first service with ports AND a non-recreate rollout, which is
	// what makes it the one the domain routes to.
	if web.Rollout != RolloutBlueGreen {
		t.Fatalf("web rollout = %q; a published port must not disqualify the web tier", web.Rollout)
	}
	if api.Rollout != RolloutBlueGreen {
		t.Fatalf("api rollout = %q", api.Rollout)
	}
	// A named volume is a real exclusivity claim and still costs the swap.
	if db.Rollout != RolloutRecreate {
		t.Fatalf("db holds a named volume and must be recreate, got %q", db.Rollout)
	}
}
