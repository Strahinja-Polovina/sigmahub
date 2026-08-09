// The compose boundary makes its own declared type true.
//
// `CpComposeServices.services` is typed `CpComposeService[]`, and the resource
// page reads `.length` on it for EVERY app it renders. A Go nil slice marshals
// to JSON `null`, so when the control plane answered "this app has no compose
// graph" — the common case, since most apps are plain Dockerfile apps — the
// page threw `Cannot read properties of null (reading 'length')` and rendered
// nothing.
//
// tsc cannot catch that: the type was a claim about a JSON body, and a claim
// about a JSON body is only worth what the parser enforces. So this file drives
// the real function against the real payload the control plane sent.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("server-only", () => ({}));
// cpGetComposeServices touches the database only to look up the org's cached
// service token, so both are stubbed — a real one would make this file about
// credential plumbing (and about PGlite's startup time) rather than about the
// shape of one JSON body.
vi.mock("@/server/db", () => ({
  db: {},
  client: {
    query: async (sql: string) =>
      /SELECT token/i.test(sql) ? { rows: [{ token: "svc_test" }] } : { rows: [] },
  },
}));

const CP_URL = "http://cp.internal:8080";

/** The body the control plane returns, steered per test. */
let payload: unknown;

beforeEach(() => {
  process.env.SIGMAHUB_CP_URL = CP_URL;
  vi.stubGlobal("fetch", async () =>
    new Response(JSON.stringify(payload), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    })
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.SIGMAHUB_CP_URL;
});

describe("the compose service graph the resource page renders", () => {
  it("turns a null service list into an empty one", async () => {
    // Verbatim what a control plane that has not been redeployed still sends.
    payload = { services: null, homeServerId: "srv_home" };
    const { cpGetComposeServices } = await import("./cp");

    const res = await cpGetComposeServices("org_1", "res_1");

    expect(res.services).toEqual([]);
    // The assertion that matters is not "it is falsy" but that the page's own
    // expression works, because that expression is what threw.
    expect(res.services.length).toBe(0);
    expect(res.homeServerId).toBe("srv_home");
  });

  it("does not flatten a real service graph while doing it", async () => {
    // A coercion that returned [] unconditionally would satisfy the test above
    // and silently empty the placement panel for every Compose app.
    payload = {
      services: [{ name: "web", image: "nginx" }, { name: "worker", build: "." }],
      homeServerId: "srv_home",
    };
    const { cpGetComposeServices } = await import("./cp");

    const res = await cpGetComposeServices("org_1", "res_1");

    expect(res.services.map((s) => s.name)).toEqual(["web", "worker"]);
  });

  it("survives a body with no compose fields at all", async () => {
    payload = {};
    const { cpGetComposeServices } = await import("./cp");

    const res = await cpGetComposeServices("org_1", "res_1");

    expect(res.services).toEqual([]);
    expect(res.homeServerId).toBe("");
  });
});
