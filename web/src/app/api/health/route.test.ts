// The deploy gate's own test (SIGMA-265).
//
// A staging rollout used to be declared successful once `/readyz` answered on
// the control plane and `/` answered on the dashboard. Neither observes the
// path every logged-in page depends on: `/readyz` pings Postgres, and `/` is
// the marketing home page, which for an anonymous request renders static
// sections without ever reading SIGMAHUB_CP_URL or the service token. So the
// deploy that dropped SIGMAHUB_CP_URL from the compose environment — the exact
// accident docker-compose.yml records having happened once already — reported
// green while the fleet was down.
//
// This route is what the rollout polls instead, so what it must NOT do is
// answer 200 when the dashboard cannot talk to the control plane. Every case
// below is a way that could happen on the box.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("server-only", () => ({}));
// The health check never reads the database; the mock only keeps importing
// @/server/cp (which opens a pool at module load) out of this file's way.
vi.mock("@/server/db", () => ({
  db: {},
  client: { query: async () => ({ rows: [] }) },
}));

const CP_URL = "http://cp.internal:8080";

/** What the stub control plane answers the credential probe with. */
let cpStatus: number;
/** Set when the stubbed fetch is meant to fail outright (CP down). */
let cpError: Error | null;
/** The requests the route made, so the probe itself can be asserted. */
let calls: Array<{ url: string; init: RequestInit | undefined }>;

beforeEach(() => {
  cpStatus = 400;
  cpError = null;
  calls = [];
  process.env.SIGMAHUB_CP_URL = CP_URL;
  process.env.SIGMAHUB_CP_PROVISION_TOKEN = "prov_real";
  vi.stubGlobal("fetch", async (url: string, init: RequestInit | undefined) => {
    calls.push({ url: String(url), init });
    if (cpError) throw cpError;
    return new Response(JSON.stringify({ error: "orgId is required" }), {
      status: cpStatus,
      headers: { "Content-Type": "application/json" },
    });
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.SIGMAHUB_CP_URL;
  delete process.env.SIGMAHUB_CP_PROVISION_TOKEN;
});

async function get(url: string) {
  const { GET } = await import("./route");
  return GET(new Request(url));
}

describe("GET /api/health?require=cp", () => {
  it("round-trips to the control plane with the dashboard's real credential", async () => {
    const res = await get("http://web/api/health?require=cp");

    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ status: "ok", cp: "ok" });
    // The point of the route is the round trip; assert it happened, against
    // the configured control plane, carrying the configured token.
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(`${CP_URL}/v1/orgs`);
    const headers = new Headers(calls[0].init?.headers);
    expect(headers.get("authorization")).toBe("Bearer prov_real");
  });

  it("fails when SIGMAHUB_CP_URL is missing from the environment", async () => {
    // The ticket's reproduction: unset the variable in the compose environment
    // and the rollout's verification stage must go red.
    delete process.env.SIGMAHUB_CP_URL;

    const res = await get("http://web/api/health?require=cp");

    expect(res.status).toBe(503);
    expect(await res.json()).toMatchObject({ status: "degraded", cp: "unconfigured" });
  });

  it("fails when the control plane rejects the dashboard's credential", async () => {
    // The service-token env var renamed, or the CP's auth middleware tightened.
    cpStatus = 401;

    const res = await get("http://web/api/health?require=cp");

    expect(res.status).toBe(503);
    expect(await res.json()).toMatchObject({ cp: "unauthorized" });
  });

  it("fails when the control plane cannot be reached at all", async () => {
    cpError = new Error("connect ECONNREFUSED");

    const res = await get("http://web/api/health?require=cp");

    expect(res.status).toBe(503);
    expect(await res.json()).toMatchObject({ cp: "unreachable" });
  });

  it("reports the control plane as disabled — without failing — when nobody asked for it", async () => {
    // Demo deployments run with no control plane on purpose. They are not
    // broken, so a bare /api/health must not say they are; the staging gate
    // states its expectation with ?require=cp instead.
    delete process.env.SIGMAHUB_CP_URL;

    const res = await get("http://web/api/health");

    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ status: "ok", cp: "disabled" });
    expect(calls).toHaveLength(0);
  });
});
