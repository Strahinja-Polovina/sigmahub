// What the Idempotency-Key the web app sends actually promises.
//
// `Idempotency-Key` means "this is the SAME submission as the one I sent a
// moment ago", and the control plane treats it that way: a matching key with a
// matching body replays the stored response instead of re-executing
// (cp/internal/api/idempotency.go). Two mistakes are possible, and the web app
// made one of each:
//
//   - a key derived from the request's CONTENT (SIGMA-253) is stable for the
//     lifetime of that content, not for one submission. Delete the cluster and
//     build it again under the same name and the control plane correctly
//     replays the FIRST submission's 201 — the dashboard navigates to a cluster
//     id that no longer exists, nothing is created, and every retry replays the
//     same dead response.
//   - a key minted per CALL (SIGMA-256) is never repeated, so the claim always
//     wins and the mutation always executes. That is precisely the
//     double-execute the wrapper exists to prevent.
//
// Both are invisible to tsc: the header is a string either way. So this file
// drives the real client and reads the header off the request.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("server-only", () => ({}));
// The client touches the database only for the org's cached service token.
vi.mock("@/server/db", () => ({
  db: {},
  client: {
    query: async (sql: string) =>
      /SELECT token/i.test(sql) ? { rows: [{ token: "svc_test" }] } : { rows: [] },
  },
}));

const CP_URL = "http://cp.internal:8080";
const ACTOR = { name: "you", role: "Org Admin" };

/** Every Idempotency-Key the client put on the wire, in order. */
let keys: (string | null)[] = [];

beforeEach(() => {
  keys = [];
  process.env.SIGMAHUB_CP_URL = CP_URL;
  vi.stubGlobal("fetch", async (_url: string, init: RequestInit) => {
    const headers = new Headers(init?.headers as HeadersInit);
    keys.push(headers.get("Idempotency-Key"));
    return new Response(JSON.stringify({ id: "cls_1", name: "prod" }), {
      status: 201,
      headers: { "Content-Type": "application/json" },
    });
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.SIGMAHUB_CP_URL;
  vi.resetModules();
});

describe("cpCreateCluster's Idempotency-Key", () => {
  const input = { environmentId: "env_x", name: "prod", controlPlaneId: "srv_1" };

  it("is the caller's request id, so retries of one submission share it", async () => {
    const { cpCreateCluster } = await import("./cp");

    await cpCreateCluster("org_1", input, ACTOR, "req_abc");
    await cpCreateCluster("org_1", input, ACTOR, "req_abc");

    expect(keys[0]).toBe(keys[1]);
    expect(keys[0]).toContain("req_abc");
  });

  it("differs between two submissions of the same cluster", async () => {
    const { cpCreateCluster } = await import("./cp");

    // The same environment, the same name, the same control-plane node — and a
    // genuinely new submission, because the operator deleted the first cluster
    // after its k3s install failed. A content-derived key makes these two
    // indistinguishable and the second one replays the deleted cluster's 201.
    await cpCreateCluster("org_1", input, ACTOR, "req_first");
    await cpCreateCluster("org_1", input, ACTOR, "req_second");

    expect(keys[0]).not.toBe(keys[1]);
  });
});
