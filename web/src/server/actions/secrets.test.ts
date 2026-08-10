// What a resubmitted New Secret form does to the control plane.
//
// SIGMA-256. `cpCreateSecret` minted `crypto.randomUUID()` for its
// Idempotency-Key — a fresh key on every CALL. A key that is never repeated is
// not an idempotency key: `IdempotencyClaim` always wins the reservation and
// the mutation always executes, which is the exact double-execute the wrapper
// exists to prevent. The only thing the header bought was one finalized
// `idempotency_keys` row per attempt, which nothing pruned.
//
// The user-visible failure is not subtle. The proxy times out after the CP has
// already committed the secret and minted its config deployments. The user sees
// an error and presses Save again. With a per-call key the CP executes a second
// CreateSecret: a second audit entry, a second round of config deployments, and
// a second container restart wave across every resource consuming that secret —
// during what the user believes was a failed operation.
//
// So the test drives the real action against a control plane that implements
// the real wrapper's rules, and counts executions.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("server-only", () => ({}));
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
vi.mock("../audit", () => ({ writeAudit: async () => {} }));
vi.mock("../active-org", () => ({
  requireMembership: async () => ({ user: { id: "usr_you", name: "you" }, role: "Org Admin" }),
  requireProjectRole: async () => ({ user: { id: "usr_you", name: "you" }, role: "Org Admin" }),
}));
vi.mock("../queries", () => ({
  getResource: async () => ({
    id: "res_1",
    name: "api",
    projectId: "prj_1",
    environmentId: "env_1",
  }),
  getProject: async () => ({ id: "prj_1", orgId: "org_1" }),
}));
// cp.ts reads the org's cached service token out of the raw client; nothing
// else here touches the database.
vi.mock("@/server/db", () => ({
  db: {},
  client: {
    query: async (sql: string) =>
      /SELECT token/i.test(sql) ? { rows: [{ token: "svc_test" }] } : { rows: [] },
  },
}));
vi.mock("../db", () => ({
  db: {},
  client: {
    query: async (sql: string) =>
      /SELECT token/i.test(sql) ? { rows: [{ token: "svc_test" }] } : { rows: [] },
  },
}));

const CP_URL = "http://cp.internal:8080";

/** Bodies of the POSTs that actually EXECUTED a create, i.e. the ones the fake
 *  control plane did not answer from its idempotency table. */
let executed: string[] = [];
/** Every POST that reached the fake control plane, replayed or not. */
let received: { key: string | null; replayed: boolean }[] = [];

/** A control plane with cp/internal/api/idempotency.go's rules and nothing
 *  else: claim the key up front, replay a finalized response for a matching
 *  body, 409 for a matching key with a different body. */
function fakeControlPlane() {
  const table = new Map<string, { hash: string; body: string }>();
  return async (url: string, init: RequestInit) => {
    const headers = new Headers(init?.headers as HeadersInit);
    const key = headers.get("Idempotency-Key");
    const body = String(init?.body ?? "");
    if (!/\/secrets$/.test(new URL(url).pathname)) {
      return new Response("{}", { status: 200 });
    }
    if (key) {
      const stored = table.get(key);
      if (stored) {
        if (stored.hash !== body) {
          received.push({ key, replayed: false });
          return new Response(JSON.stringify({ error: "key reused with a different request" }), {
            status: 409,
          });
        }
        received.push({ key, replayed: true });
        return new Response(stored.body, {
          status: 201,
          headers: { "Idempotency-Replayed": "true", "Content-Type": "application/json" },
        });
      }
    }
    // No stored response: the mutation runs.
    executed.push(body);
    received.push({ key, replayed: false });
    const response = JSON.stringify({ id: `sec_${executed.length}`, name: "DATABASE_URL" });
    if (key) table.set(key, { hash: body, body: response });
    return new Response(response, {
      status: 201,
      headers: { "Content-Type": "application/json" },
    });
  };
}

beforeEach(() => {
  executed = [];
  received = [];
  process.env.SIGMAHUB_CP_URL = CP_URL;
  vi.stubGlobal("fetch", fakeControlPlane());
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.SIGMAHUB_CP_URL;
  vi.resetModules();
});

const FORM = {
  resourceId: "res_1",
  name: "DATABASE_URL",
  value: "postgres://user:pw@db:5432/app",
  scope: "environment" as const,
  envVar: true,
};

describe("createSecretAction", () => {
  it("a retried secret submission executes CreateSecret once", async () => {
    const { createSecretAction } = await import("./secrets");

    // Same form, same submission — the user pressed Save twice because the
    // first attempt appeared to fail.
    await createSecretAction({ ...FORM, requestId: "req_form" });
    await createSecretAction({ ...FORM, requestId: "req_form" });

    expect(received).toHaveLength(2);
    expect(executed).toHaveLength(1);
    expect(received[1].replayed).toBe(true);
  });

  it("a second, separate submission is not swallowed as a replay", async () => {
    const { createSecretAction } = await import("./secrets");

    // A different secret entirely, and a form that minted a new id for it. If
    // the retry fix over-corrected into a content-derived key, this would come
    // back as a replay of the first secret's 201 (the SIGMA-253 failure).
    await createSecretAction({ ...FORM, requestId: "req_one" });
    await createSecretAction({ ...FORM, name: "REDIS_URL", requestId: "req_two" });

    expect(executed).toHaveLength(2);
    expect(received.every((r) => !r.replayed)).toBe(true);
  });
});
