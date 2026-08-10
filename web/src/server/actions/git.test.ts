// SIGMA-334: the git read actions must know who is calling BEFORE they spend
// the provision token on the orgId the caller typed.
//
// getOrgToken provisions on first use: no row in cp_org_tokens for an orgId
// means POST /v1/orgs with the deployment-wide provision credential, and the
// Org Admin service token that comes back is persisted — in cp_org_tokens here
// and in service_tokens on the control plane. That is the right behaviour for
// org creation and wrong for a GET, because listPreviews and
// requireConnectionAdmin used to call cpGetGitConnection FIRST and authorize
// second. Any signed-in user could therefore loop the action over orgIds they
// have nothing to do with and leave a live Org Admin credential and an empty
// org behind for each one, attributed to nobody. The SIGMA-93 IDOR fix was
// real, it just ran one call too late.
//
// So these tests assert on the SIDE EFFECT, not on the thrown error: the action
// rejecting proves nothing (it already did), while a cp_org_tokens row and a
// POST /v1/orgs on the wire are the damage. The control plane is a fetch stub
// and the database is the real migrated schema (PGlite) — cp.ts's token cache
// is raw SQL against `client`, so it has to be a real table.

import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";

import * as s from "@/server/db/schema";
import { user as authUser } from "@/server/db/auth-schema";
import type { DemoDb } from "@/server/testing/demo-db";

const ORG = "org_acme";
const SHOP = "proj_shop";
/** An org the caller has no membership in — the id an enumerating loop passes. */
const FOREIGN_ORG = "x0001";

const MEMBER = { id: "usr_mia", name: "Mia Member", email: "mia@acme.example" };

let sessionUser: { id: string; name: string; email: string } = MEMBER;

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  const db = await createDemoDb();
  // cp.ts keeps its org-token cache in raw SQL through `client`, so the mock
  // has to expose the same PGlite instance drizzle is talking to — otherwise
  // the test would be watching a different database than the code writes to.
  const pg = (db as unknown as { $client: RawPg }).$client;
  return {
    db,
    client: { query: (sql: string, params?: unknown[]) => pg.query(sql, params), close: () => pg.close() },
  };
});
type RawPg = {
  query<T = Record<string, unknown>>(sql: string, params?: unknown[]): Promise<{ rows: T[] }>;
  close(): Promise<void>;
};

vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
// active-org is NOT mocked: whether the membership check runs before the CP
// call is the whole question. Only identity underneath it is faked.
vi.mock("next/headers", () => ({
  headers: async () => new Headers(),
  cookies: async () => ({ get: () => undefined, set: () => {} }),
}));
vi.mock("next/navigation", () => ({
  redirect: () => {
    throw new Error("redirected to /login");
  },
}));
vi.mock("@/lib/auth", () => ({
  auth: { api: { getSession: async () => ({ user: sessionUser }) } },
}));

process.env.SIGMAHUB_CP_URL = "http://control-plane.test";
process.env.SIGMAHUB_CP_PROVISION_TOKEN = "test-provision-token";

const { db, client } = (await import("@/server/db")) as unknown as {
  db: DemoDb;
  client: RawPg;
};
const { listPreviews, setPreviews } = await import("@/server/actions/git");

/** Every request the actions made of the control plane, as "METHOD /path". */
let calls: string[] = [];

/** A control plane that will happily provision anything it is asked to — the
 *  point being that a well-behaved caller never asks. */
function stubCp() {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      calls.push(`${method} ${url.pathname}`);
      const json = (body: unknown, status = 200) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      if (method === "POST" && url.pathname === "/v1/orgs") {
        return json({ orgId: "provisioned", token: `sst_${Date.now()}` }, 201);
      }
      if (url.pathname.endsWith("/previews")) {
        return json({ previews: [] });
      }
      if (url.pathname.includes("/git/connections/")) {
        return json({ connection: { id: "conn_1", projectId: SHOP }, branchMaps: [] });
      }
      return json({ error: "not found" }, 404);
    })
  );
}

/** Rows cp.ts cached for an org. The table is created here rather than left to
 *  ensureOrgTokenTable so the count is answerable even when — as it should be
 *  for a non-member — the code never gets far enough to create it. */
async function cachedTokens(orgId: string): Promise<number> {
  const res = await client.query<{ n: string }>(
    `SELECT count(*)::text AS n FROM cp_org_tokens WHERE org_id = $1`,
    [orgId]
  );
  return Number(res.rows[0]?.n ?? "0");
}

beforeEach(async () => {
  sessionUser = MEMBER;
  stubCp();
  await client.query(`CREATE TABLE IF NOT EXISTS cp_org_tokens (
    org_id     TEXT PRIMARY KEY,
    token      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
  )`);
  await client.query(`DELETE FROM cp_org_tokens`);
  await db.delete(s.projectMemberships);
  await db.delete(s.memberships);
  await db.delete(s.projects);
  await db.delete(s.orgs);
  await db.delete(authUser);

  const now = new Date();
  await db.insert(authUser).values({
    id: MEMBER.id,
    name: MEMBER.name,
    email: MEMBER.email,
    emailVerified: true,
    createdAt: now,
    updatedAt: now,
  });
  await db.insert(s.orgs).values({ id: ORG, name: "Acme", slug: "acme" });
  await db.insert(s.projects).values({ id: SHOP, orgId: ORG, name: "Shop", slug: "shop" });
  await db.insert(s.memberships).values({
    id: "mem_mia",
    orgId: ORG,
    userId: MEMBER.id,
    role: "Project Admin",
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("git read actions and the provision token", () => {
  it("listPreviews does not provision a CP org for a non-member", async () => {
    await expect(
      listPreviews({ orgId: FOREIGN_ORG, connectionId: "junk" })
    ).rejects.toThrow(/not a member/i);

    // The damage this ticket is about: a live Org Admin credential minted for
    // an org the caller has no relationship with, on a read.
    expect(await cachedTokens(FOREIGN_ORG)).toBe(0);
    expect(calls).not.toContain("POST /v1/orgs");
    // And nothing was read from the control plane either.
    expect(calls).toEqual([]);
  });

  it("setPreviews does not provision a CP org for a non-member", async () => {
    await expect(
      setPreviews({
        orgId: FOREIGN_ORG,
        projectId: SHOP,
        connectionId: "junk",
        enabled: true,
      })
    ).rejects.toThrow(/not a member/i);

    expect(await cachedTokens(FOREIGN_ORG)).toBe(0);
    expect(calls).toEqual([]);
  });

  it("still serves a member of the org", async () => {
    await expect(listPreviews({ orgId: ORG, connectionId: "conn_1" })).resolves.toEqual([]);
    // Provisioning on first use is intended behaviour — for somebody who
    // belongs to the org.
    expect(calls).toContain("POST /v1/orgs");
    expect(await cachedTokens(ORG)).toBe(1);
  });
});
