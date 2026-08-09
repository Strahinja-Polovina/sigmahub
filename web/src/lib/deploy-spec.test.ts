import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  ROLLOUT_BLUE_GREEN,
  ROLLOUT_RECREATE,
  buildResourceSpec,
  composeSpecFromDetected,
  createResourceBody,
  ignoredHostPorts,
  recreateReason,
  recreateSummary,
  resolveDeployTarget,
  type DetectedComposeService,
} from "./deploy-spec";

// The five-service repo that used to become one container: a source-built web
// tier, a worker with no ports, a prebuilt cache, a database holding a named
// volume, and a proxy binding fixed host ports.
const FIVE: DetectedComposeService[] = [
  { name: "web", build: ".", dockerfile: "Dockerfile", ports: [8080], dependsOn: ["db", "cache"], rollout: ROLLOUT_BLUE_GREEN },
  { name: "worker", build: "./worker", rollout: ROLLOUT_BLUE_GREEN },
  { name: "cache", image: "redis:7.4", ports: [6379], rollout: ROLLOUT_BLUE_GREEN },
  { name: "db", image: "postgres:16", ports: [5432], namedVolumes: ["pgdata"], rollout: ROLLOUT_RECREATE },
  { name: "proxy", image: "traefik:3", ports: [80, 443], publishedPorts: [80, 443], rollout: ROLLOUT_RECREATE },
];

describe("composeSpecFromDetected", () => {
  it("carries every detected service into the spec", () => {
    const spec = composeSpecFromDetected(FIVE);
    expect(spec?.services.map((s) => s.name)).toEqual(["web", "worker", "cache", "db", "proxy"]);
  });

  it("keeps every field the renderer builds from", () => {
    const web = composeSpecFromDetected(FIVE)?.services.find((s) => s.name === "web");
    // build + dockerfile drive image.build; ports drive the exposed port AND the
    // service's TCP readiness probe; dependsOn becomes op ordering. Any one of
    // them missing is a service that builds or starts wrongly, not a display bug.
    expect(web).toEqual({
      name: "web",
      build: ".",
      dockerfile: "Dockerfile",
      ports: [8080],
      dependsOn: ["db", "cache"],
      rollout: ROLLOUT_BLUE_GREEN,
    });
  });

  it("keeps the evidence behind a recreate verdict", () => {
    const services = composeSpecFromDetected(FIVE)?.services ?? [];
    expect(services.find((s) => s.name === "db")?.namedVolumes).toEqual(["pgdata"]);
    expect(services.find((s) => s.name === "proxy")?.publishedPorts).toEqual([80, 443]);
  });

  it("does not filter a service the renderer would skip", () => {
    // A service with neither build nor image cannot run, and the reconciler
    // drops it at render time. Dropping it HERE would make the stored spec
    // disagree with the repository — the exact silent loss this replaces.
    const spec = composeSpecFromDetected([
      { name: "web", build: ".", rollout: ROLLOUT_BLUE_GREEN },
      { name: "notes", rollout: ROLLOUT_BLUE_GREEN },
    ]);
    expect(spec?.services.map((s) => s.name)).toEqual(["web", "notes"]);
  });

  it("writes a rollout verdict even when detection omitted one", () => {
    const spec = composeSpecFromDetected([{ name: "web", build: "." }]);
    expect(spec?.services[0].rollout).toBe(ROLLOUT_BLUE_GREEN);
  });

  it("is null for a plain Dockerfile app", () => {
    expect(composeSpecFromDetected(undefined)).toBeNull();
    expect(composeSpecFromDetected([])).toBeNull();
    // Nameless services are unaddressable, so a graph of only those is no graph.
    expect(composeSpecFromDetected([{ name: "  " }])).toBeNull();
  });

  it("does not alias the detected arrays", () => {
    const detected: DetectedComposeService[] = [{ name: "db", image: "postgres:16", ports: [5432] }];
    const spec = composeSpecFromDetected(detected);
    spec?.services[0].ports?.push(9999);
    expect(detected[0].ports).toEqual([5432]);
  });
});

describe("recreate is explained, not just flagged", () => {
  it("names the named volume", () => {
    expect(recreateReason(FIVE[3])).toBe("it mounts the named volume pgdata");
  });

  // A host port is NOT a reason for downtime, and saying it was is the defect
  // this replaced: SigmaHub never binds the host port, so the service was told
  // it would be stopped for a binding its container does not have.
  it("does not blame a host port for downtime", () => {
    expect(
      recreateReason({
        name: "db",
        image: "postgres:16",
        namedVolumes: ["pgdata"],
        publishedPorts: [5432],
        rollout: ROLLOUT_RECREATE,
      })
    ).toBe("it mounts the named volume pgdata");
  });

  it("reports ignored host bindings instead of hiding them", () => {
    expect(
      ignoredHostPorts([
        { name: "web", build: ".", publishedPorts: [3000], rollout: ROLLOUT_BLUE_GREEN },
        { name: "api", build: "./api", rollout: ROLLOUT_BLUE_GREEN },
      ])
    ).toEqual([{ name: "web", ports: [3000] }]);
    expect(ignoredHostPorts(undefined)).toEqual([]);
  });

  it("still says something true when the evidence did not survive", () => {
    expect(recreateReason({ name: "db", image: "postgres:16", rollout: ROLLOUT_RECREATE })).toBe(
      "it holds a resource only one copy can own at a time"
    );
  });

  it("says nothing about a blue-green service", () => {
    expect(recreateReason(FIVE[0])).toBeNull();
  });

  it("summarises exactly the services that will go down", () => {
    expect(recreateSummary(FIVE).map((s) => s.name)).toEqual(["db", "proxy"]);
    expect(recreateSummary([FIVE[0], FIVE[1]])).toEqual([]);
    expect(recreateSummary(undefined)).toEqual([]);
  });
});

// The whole defect was a web type that had FEWER fields than the wire it reads
// from: the CP sent the Compose graph on every detect, and the dashboard's type
// simply had nowhere to put it, so it was parsed and discarded in silence. A
// missing field cannot be a type error — nothing in TypeScript compares a
// structural type to a Go struct — so the mirror is only real if something
// checks it.
describe("the detected-service type mirrors the control plane's struct", () => {
  const REPO = join(process.cwd(), "..");

  /** Field names from a Go struct's `json:"…"` tags. */
  function goJSONFields(file: string, structName: string): string[] {
    const src = readFileSync(join(REPO, file), "utf8");
    const start = src.indexOf(`type ${structName} struct {`);
    expect(start, `${structName} not found in ${file}`).toBeGreaterThan(-1);
    const end = src.indexOf("\n}", start);
    const body = src.slice(start, end);
    const out: string[] = [];
    for (const line of body.split("\n")) {
      if (line.trimStart().startsWith("//")) continue;
      const m = /json:"([^",]+)/.exec(line);
      if (m && m[1] !== "-") out.push(m[1]);
    }
    return out.sort();
  }

  /** Property names from a TS object-type alias. */
  function tsFields(file: string, typeName: string): string[] {
    const src = readFileSync(join(process.cwd(), file), "utf8");
    const start = src.indexOf(`export type ${typeName} = {`);
    expect(start, `${typeName} not found in ${file}`).toBeGreaterThan(-1);
    const end = src.indexOf("\n};", start);
    const body = src.slice(start, end);
    const out: string[] = [];
    for (const line of body.split("\n").slice(1)) {
      const t = line.trimStart();
      // Skip doc comments; a prose colon is not a property.
      if (t.startsWith("*") || t.startsWith("/")) continue;
      const m = /^(\w+)\??:/.exec(t);
      if (m) out.push(m[1]);
    }
    return out.sort();
  }

  const go = goJSONFields("cp/internal/gitdetect/compose.go", "ComposeService");

  it("names every field gitdetect emits, and no invented ones", () => {
    expect(
      tsFields("src/server/cp.ts", "CpDetectedComposeService"),
      "CpDetectedComposeService must mirror gitdetect.ComposeService field for field"
    ).toEqual(go);
  });

  it("keeps the lib copy in step with the wire type", () => {
    expect(
      tsFields("src/lib/deploy-spec.ts", "DetectedComposeService"),
      "DetectedComposeService is the server-free restatement of the same shape"
    ).toEqual(go);
  });

  it("actually found a struct to compare against", () => {
    // A silently-empty extraction would make both assertions above vacuous.
    expect(go).toContain("name");
    expect(go).toContain("rollout");
    expect(go.length).toBeGreaterThan(5);
  });
});

describe("resolveDeployTarget", () => {
  it("accepts a server", () => {
    expect(resolveDeployTarget({ serverId: "srv_1" })).toEqual({ ok: true, serverId: "srv_1" });
  });

  it("accepts a cluster", () => {
    expect(resolveDeployTarget({ clusterId: "cls_1" })).toEqual({ ok: true, clusterId: "cls_1" });
  });

  it("refuses both, and says which mistake was made", () => {
    const got = resolveDeployTarget({ serverId: "srv_1", clusterId: "cls_1" });
    expect(got.ok).toBe(false);
    expect(got.ok === false && got.error).toMatch(/not both/);
  });

  it("refuses neither, and offers both options", () => {
    const got = resolveDeployTarget({ serverId: null, clusterId: undefined });
    expect(got.ok).toBe(false);
    // The old message was "A target server is required", which named the wrong
    // rule: it made a legitimate cluster deploy look like a client bug.
    expect(got.ok === false && got.error).toMatch(/server/);
    expect(got.ok === false && got.error).toMatch(/cluster/);
  });

  it("treats whitespace as absent", () => {
    expect(resolveDeployTarget({ serverId: "   ", clusterId: "cls_1" })).toEqual({
      ok: true,
      clusterId: "cls_1",
    });
    expect(resolveDeployTarget({ serverId: " ", clusterId: " " }).ok).toBe(false);
  });
});

// The wiring, not the helpers.
//
// Both SIGMA-199 and SIGMA-200 were one dropped assignment each, and the first
// version of this fix was protected by nothing: deleting the statement that
// writes `spec.compose` left lint, tsc and every test green. These assert the
// spec a compose repo actually produces, so the fix cannot be removed silently.
describe("the resource spec carries what was detected", () => {
  const services: DetectedComposeService[] = [
    { name: "web", build: ".", ports: [3000], publishedPorts: [3000], dependsOn: ["api"], rollout: ROLLOUT_BLUE_GREEN },
    { name: "api", build: "./api", dockerfile: "Dockerfile.prod", ports: [8080], rollout: ROLLOUT_BLUE_GREEN },
    { name: "db", image: "postgres:16", namedVolumes: ["pgdata"], rollout: ROLLOUT_RECREATE },
  ];

  it("turns a detected compose graph into spec.compose", () => {
    const spec = buildResourceSpec({
      repo: "acme/shop",
      detected: { ports: [3000], services },
    });
    const compose = spec.compose as { services: DetectedComposeService[] } | undefined;
    expect(compose, "a compose repo without spec.compose deploys as ONE container").toBeDefined();
    expect(compose!.services.map((s) => s.name)).toEqual(["web", "api", "db"]);
    // The fields the reconciler needs to build and place each service must
    // survive; a dropped dockerfile silently rebuilds from the wrong file.
    expect(compose!.services[1].dockerfile).toBe("Dockerfile.prod");
    expect(compose!.services[2].namedVolumes).toEqual(["pgdata"]);
    expect(compose!.services[0].dependsOn).toEqual(["api"]);
  });

  it("leaves spec.compose off a single-container repo", () => {
    const spec = buildResourceSpec({ repo: "acme/api", detected: { ports: [8080] } });
    expect(spec.compose).toBeUndefined();
    expect(spec.ports).toEqual([{ container: 8080, host: 0, protocol: "tcp" }]);
  });

  it("defaults every port to internal-only", () => {
    const spec = buildResourceSpec({ detected: { ports: [80, 443] } });
    // host 0 is what keeps a new app off the public internet until a domain is
    // attached deliberately.
    for (const p of spec.ports as { host: number }[]) {
      expect(p.host).toBe(0);
    }
  });

  it("drops impossible ports rather than persisting them", () => {
    const spec = buildResourceSpec({ detected: { ports: [0, -1, 70000, 8080] } });
    expect(spec.ports).toEqual([{ container: 8080, host: 0, protocol: "tcp" }]);
  });
});

// SIGMA-200 was a field missing from this body, and it stayed missing because
// nothing on the web side asserted what goes on the wire.
describe("the create-resource request carries its target", () => {
  it("sends a cluster target", () => {
    const body = createResourceBody({
      environmentId: "env_1",
      clusterId: "cls_1",
      name: "api",
      kind: "app",
    });
    expect(body.clusterId, "a cluster target that never reaches the CP is SIGMA-200").toBe("cls_1");
    // Both keys always present: an omitted key and an empty one must not mean
    // different things to the exactly-one-target check.
    expect(body.serverId).toBe("");
  });

  it("sends a server target", () => {
    const body = createResourceBody({
      environmentId: "env_1",
      serverId: "srv_1",
      name: "api",
      kind: "app",
    });
    expect(body.serverId).toBe("srv_1");
    expect(body.clusterId).toBe("");
  });
});

// The build block is the SIGMA-209 wiring, and it has exactly the shape that
// went wrong four times before: a couple of fields that every layer below
// already understood and nothing above ever set. The agent's image.build op has
// taken `dockerfile` and `contextSubdir` since the Compose work; because the
// single-container path never wrote them, a repo whose Dockerfile is not at its
// root simply could not be deployed. Deleting any assertion below makes the
// feature disappear while the pure helpers on either side stay green.
describe("the persisted spec carries the build decision", () => {
  it("writes the method, the Dockerfile path and the build context", () => {
    const spec = buildResourceSpec({
      repo: "acme/platform",
      build: { method: "dockerfile", dockerfile: "Dockerfile", contextSubdir: "apps/api" },
    });
    expect(
      spec.build,
      "without spec.build a monorepo builds at the repo root and finds nothing"
    ).toEqual({ method: "dockerfile", dockerfile: "Dockerfile", contextSubdir: "apps/api" });
  });

  it("writes the auto-build method for a repo with no Dockerfile", () => {
    const spec = buildResourceSpec({
      repo: "acme/reporting",
      build: { method: "nixpacks", contextSubdir: "services/api" },
    });
    expect(spec.build).toEqual({ method: "nixpacks", contextSubdir: "services/api" });
  });

  // Every resource created before the wizard could express any of this.
  it("writes no build block when nothing was decided", () => {
    expect(buildResourceSpec({ repo: "acme/shop" }).build).toBeUndefined();
  });
});

// Detection is a PRE-FILL the user is shown precisely so they can correct it.
// Re-deriving the ports from the raw detected list at create time would
// silently discard the correction — and the published host port that came with
// it (SIGMA-210).
describe("the wizard's port mappings beat the detected ones", () => {
  it("uses the mappings the user left the networking step with", () => {
    const spec = buildResourceSpec({
      repo: "acme/shop",
      detected: { ports: [3000] },
      ports: [{ container: 8080, host: 8080 }],
    });
    expect(spec.ports).toEqual([{ container: 8080, host: 8080, protocol: "tcp" }]);
  });

  it("still falls back to detection when the flow collected none", () => {
    const spec = buildResourceSpec({ repo: "acme/shop", detected: { ports: [3000] } });
    expect(spec.ports).toEqual([{ container: 3000, host: 0, protocol: "tcp" }]);
  });

  it("clamps a nonsense host port to internal-only rather than sending it", () => {
    const spec = buildResourceSpec({
      repo: "acme/shop",
      ports: [{ container: 3000, host: 99999 }],
    });
    expect(spec.ports).toEqual([{ container: 3000, host: 0, protocol: "tcp" }]);
  });
});

// Object storage picks an engine through the SAME `engine` spec field the LLM
// runtime uses, because that is what the control plane reads for both.
describe("the object-storage engine reaches the spec", () => {
  it("writes the chosen engine", () => {
    expect(buildResourceSpec({ s3Engine: "seaweedfs" }).engine).toBe("seaweedfs");
  });

  it("leaves it unset when the flow did not ask", () => {
    expect(buildResourceSpec({ repo: "acme/shop" }).engine).toBeUndefined();
  });
});
