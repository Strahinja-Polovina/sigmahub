import "server-only";

// Control-plane client (P0-7, extended by P1-1). When SIGMAHUB_CP_URL is set
// the servers vertical and the domain-model writes talk to the real Go
// control plane instead of the simulated PGlite tables; with the flag unset
// the v1 demo path is untouched.
//
// P1-1 credential model: every org gets its own Org Admin service token,
// minted once via POST /v1/orgs (provision token) and cached in the local
// cp_org_tokens table — the wildcard dev token is no longer sent on org
// calls. Mutations attach the acting user as a signed header the CP verifies
// and audits (the actor can only narrow the token's role).

import { createHmac } from "node:crypto";
import { db, client } from "./db";
import * as schema from "./db/schema";
import type * as s from "./db/schema";

/** The acting user forwarded to the CP on mutations. */
export type CpActor = { name: string; role: string };

export type CpServer = {
  id: string;
  orgId: string;
  name: string;
  type: string;
  provider: string;
  region: string;
  status: string;
  agentVersion: string;
  facts: {
    hostname?: string;
    os?: string;
    arch?: string;
    kernel?: string;
    numCpu?: number;
    memTotalMb?: number;
  } | null;
  meshIp: string | null;
  pubkey: string | null;
  lastSeenAt: string | null;
  createdAt: string;
};

export type CpMetricPoint = {
  cpuPct: number;
  memPct: number;
  diskPct: number;
  load1: number;
  recordedAt: string;
};

export function cpEnabled(): boolean {
  return Boolean(process.env.SIGMAHUB_CP_URL);
}

function cpBase(): string {
  const url = process.env.SIGMAHUB_CP_URL;
  if (!url) throw new Error("SIGMAHUB_CP_URL is not set.");
  return url.replace(/\/$/, "");
}

/** Endpoint the agent host should dial — defaults to the same URL the web
 *  app uses, overridable when the CP sits behind a different public name. */
export function cpPublicUrl(): string {
  return (process.env.SIGMAHUB_CP_PUBLIC_URL ?? cpBase()).replace(/\/$/, "");
}

// ── Org credential store (P1-1) ─────────────────────────────────────────────

let orgTokenTableReady = false;
async function ensureOrgTokenTable() {
  if (orgTokenTableReady) return;
  // CP-mode-only infrastructure, created lazily so the demo path never touches
  // it and pre-existing dev databases self-heal without a migration step.
  await client.query(`CREATE TABLE IF NOT EXISTS cp_org_tokens (
    org_id     TEXT PRIMARY KEY,
    token      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
  )`);
  orgTokenTableReady = true;
}

/** Resolve the org's CP service token, provisioning the org on first use —
 *  which also self-heals orgs that predate the provisioning hook. */
async function getOrgToken(orgId: string): Promise<string> {
  await ensureOrgTokenTable();
  const existing = await client.query<{ token: string }>(
    `SELECT token FROM cp_org_tokens WHERE org_id = $1`,
    [orgId]
  );
  if (existing.rows[0]?.token) return existing.rows[0].token;

  const provisionToken =
    process.env.SIGMAHUB_CP_PROVISION_TOKEN ?? "dev-provision-token";
  const res = await fetch(`${cpBase()}/v1/orgs`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${provisionToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ orgId, name: `web:${orgId}` }),
    cache: "no-store",
  });
  if (!res.ok) {
    throw new Error(`Control plane org provisioning failed (${res.status})`);
  }
  const { token } = (await res.json()) as { token: string };
  await client.query(
    `INSERT INTO cp_org_tokens (org_id, token) VALUES ($1, $2)
     ON CONFLICT (org_id) DO NOTHING`,
    [orgId, token]
  );
  // A concurrent provisioner may have won the insert; use the stored winner so
  // the web app converges on one credential per org.
  const winner = await client.query<{ token: string }>(
    `SELECT token FROM cp_org_tokens WHERE org_id = $1`,
    [orgId]
  );
  return winner.rows[0]?.token ?? token;
}

/** Signed actor headers: HMAC-SHA256 over the base64url payload, keyed with
 *  the bearer token both ends already share. */
function actorHeaders(actor: CpActor, bearerToken: string): Record<string, string> {
  const payload = Buffer.from(
    JSON.stringify({ name: actor.name, role: actor.role })
  ).toString("base64url");
  const sig = createHmac("sha256", bearerToken).update(payload).digest("hex");
  return { "X-Sigmahub-Actor": payload, "X-Sigmahub-Actor-Signature": sig };
}

type CpFetchOpts = {
  /** Org whose token authenticates the call. */
  orgId: string;
  /** Acting user, forwarded signed on mutations for per-user RBAC + audit. */
  actor?: CpActor;
  /** Idempotency key for POSTs. */
  idempotencyKey?: string;
};

async function cpFetch<T>(path: string, init: RequestInit | undefined, opts: CpFetchOpts): Promise<T> {
  const token = await getOrgToken(opts.orgId);
  const res = await fetch(`${cpBase()}${path}`, {
    ...init,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...(opts.actor ? actorHeaders(opts.actor, token) : {}),
      ...(opts.idempotencyKey ? { "Idempotency-Key": opts.idempotencyKey } : {}),
      ...init?.headers,
    },
    cache: "no-store",
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    let message = `Control plane ${res.status}`;
    try {
      const parsed = JSON.parse(body) as { error?: string };
      if (parsed.error) message = `${message}: ${parsed.error}`;
    } catch {
      message = `${message}: ${body.slice(0, 200)}`;
    }
    throw new Error(message);
  }
  return res.json() as Promise<T>;
}

export async function cpListServers(orgId: string): Promise<CpServer[]> {
  const { servers } = await cpFetch<{ servers: CpServer[] }>(
    `/v1/orgs/${encodeURIComponent(orgId)}/servers`, undefined, { orgId }
  );
  return servers;
}

export async function cpGetServer(
  orgId: string,
  serverId: string
): Promise<CpServer | null> {
  try {
    return await cpFetch<CpServer>(
      `/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}`,
      undefined, { orgId }
    );
  } catch (err) {
    if (err instanceof Error && err.message.startsWith("Control plane 404")) {
      return null;
    }
    throw err;
  }
}

export async function cpServerMetrics(
  orgId: string,
  serverId: string
): Promise<CpMetricPoint[]> {
  const { points } = await cpFetch<{ points: CpMetricPoint[] }>(
    `/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/metrics`,
    undefined, { orgId }
  );
  return points;
}

export async function cpIssueBootstrapToken(
  orgId: string,
  input: { name: string; type: string; provider: string; region: string },
  actor?: CpActor
): Promise<{ token: string; expiresAt: string }> {
  return cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/bootstrap-tokens`, {
    method: "POST",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

// ── Domain model (P1-1) ─────────────────────────────────────────────────────

export type CpProject = {
  id: string;
  orgId: string;
  name: string;
  description: string;
  createdBy: string;
  createdAt: string;
};

export type CpEnvironment = {
  id: string;
  orgId: string;
  projectId: string;
  name: string;
  production: boolean;
  createdAt: string;
};

export type CpResource = {
  id: string;
  orgId: string;
  projectId: string;
  environmentId: string;
  serverId: string;
  name: string;
  kind: string;
  spec: Record<string, unknown>;
  status: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
};

export type CpAuditEntry = {
  id: number;
  orgId: string;
  actor: string;
  action: string;
  target: string;
  createdAt: string;
};

const org = (orgId: string) => `/v1/orgs/${encodeURIComponent(orgId)}`;

export async function cpCreateProject(
  orgId: string,
  input: { name: string; description?: string },
  actor: CpActor
): Promise<CpProject> {
  return cpFetch(`${org(orgId)}/projects`, {
    method: "POST",
    body: JSON.stringify({ name: input.name, description: input.description ?? "" }),
  }, { orgId, actor });
}

export async function cpUpdateProject(
  orgId: string,
  projectId: string,
  input: { name: string; description?: string },
  actor: CpActor
): Promise<CpProject> {
  return cpFetch(`${org(orgId)}/projects/${encodeURIComponent(projectId)}`, {
    method: "PATCH",
    body: JSON.stringify({ name: input.name, description: input.description ?? "" }),
  }, { orgId, actor });
}

export async function cpDeleteProject(orgId: string, projectId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/projects/${encodeURIComponent(projectId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export async function cpListProjects(orgId: string): Promise<CpProject[]> {
  const { projects } = await cpFetch<{ projects: CpProject[] }>(
    `${org(orgId)}/projects`, undefined, { orgId });
  return projects;
}

export async function cpCreateEnvironment(
  orgId: string,
  projectId: string,
  input: { name: string; production: boolean },
  actor: CpActor
): Promise<CpEnvironment> {
  return cpFetch(`${org(orgId)}/projects/${encodeURIComponent(projectId)}/environments`, {
    method: "POST",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

export async function cpDeleteEnvironment(orgId: string, envId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/environments/${encodeURIComponent(envId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export async function cpAttachServer(orgId: string, envId: string, serverId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/environments/${encodeURIComponent(envId)}/servers`, {
    method: "POST",
    body: JSON.stringify({ serverId }),
  }, { orgId, actor });
}

export async function cpDetachServer(orgId: string, envId: string, serverId: string, actor: CpActor): Promise<void> {
  await cpFetch(
    `${org(orgId)}/environments/${encodeURIComponent(envId)}/servers/${encodeURIComponent(serverId)}`,
    { method: "DELETE" }, { orgId, actor });
}

export async function cpCreateResource(
  orgId: string,
  input: {
    environmentId: string;
    serverId: string;
    name: string;
    kind: string;
    spec?: Record<string, unknown>;
  },
  actor: CpActor
): Promise<CpResource> {
  return cpFetch(`${org(orgId)}/resources`, {
    method: "POST",
    body: JSON.stringify({ ...input, spec: input.spec ?? {} }),
  }, { orgId, actor });
}

export async function cpDeleteResource(orgId: string, resourceId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

// Server + token lifecycle (P1-4). Server delete tombstones the CP record and
// revokes its agent token; a 409 (with the bound-resource list) surfaces as a
// thrown "Control plane 409" error the caller can show.
export async function cpDeleteServer(orgId: string, serverId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/servers/${encodeURIComponent(serverId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export type CpServiceToken = {
  id: string;
  name: string;
  role: string;
  createdBy: string;
  createdAt: string;
  lastUsedAt: string | null;
  revokedAt: string | null;
};

export async function cpListServiceTokens(orgId: string): Promise<CpServiceToken[]> {
  const { tokens } = await cpFetch<{ tokens: CpServiceToken[] }>(
    `${org(orgId)}/service-tokens`,
    undefined,
    { orgId }
  );
  return tokens;
}

export async function cpRotateServiceToken(
  orgId: string,
  tokenId: string,
  actor: CpActor
): Promise<{ token: string; id: string; name: string; role: string }> {
  return cpFetch(`${org(orgId)}/service-tokens/${encodeURIComponent(tokenId)}/rotate`, {
    method: "POST",
  }, { orgId, actor });
}

export async function cpRevokeServiceToken(orgId: string, tokenId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/service-tokens/${encodeURIComponent(tokenId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

// Two-phase destructive-op confirm (P1-3). Phase 1 mints a short-lived,
// single-use token authorising exactly one destructive op on one server; phase
// 2 presents that token back to execute it. Both are Project Admin+ and audited
// by the CP against the signed actor.
export async function cpRequestConfirmToken(
  orgId: string,
  input: { serverId: string; opKind: string; target: string },
  actor: CpActor
): Promise<{ token: string; expiresAt: string }> {
  return cpFetch(`${org(orgId)}/servers/${encodeURIComponent(input.serverId)}/confirm-tokens`, {
    method: "POST",
    body: JSON.stringify({ opKind: input.opKind, target: input.target }),
  }, { orgId, actor });
}

export async function cpConfirmDestructive(
  orgId: string,
  input: { serverId: string; token: string; opKind: string; target: string },
  actor: CpActor
): Promise<void> {
  await cpFetch(`${org(orgId)}/servers/${encodeURIComponent(input.serverId)}/destructive-ops`, {
    method: "POST",
    body: JSON.stringify({ token: input.token, opKind: input.opKind, target: input.target }),
  }, { orgId, actor });
}

export async function cpListResources(orgId: string, environmentId?: string): Promise<CpResource[]> {
  const qs = environmentId ? `?environmentId=${encodeURIComponent(environmentId)}` : "";
  const { resources } = await cpFetch<{ resources: CpResource[] }>(
    `${org(orgId)}/resources${qs}`, undefined, { orgId });
  return resources;
}

export async function cpListAudit(orgId: string, limit = 50): Promise<CpAuditEntry[]> {
  const { entries } = await cpFetch<{ entries: CpAuditEntry[] }>(
    `${org(orgId)}/audit?limit=${limit}`, undefined, { orgId });
  return entries;
}

/** The CP kind vocabulary says "mongodb"; the local demo schema says "mongo". */
export function cpKind(localKind: string): string {
  return localKind === "mongo" ? "mongodb" : localKind;
}

/** Mirror a CP-owned server into the local `servers` table so the mirror
 *  env_servers/resources rows (which FK to servers.id) stay referentially
 *  intact in CP mode, where servers live only in the control plane. Returns
 *  the mapped row; throws if the CP doesn't recognise the id for this org
 *  (restoring the IDOR guard the local lookup used to provide). */
export async function cpMirrorServer(
  orgId: string,
  serverId: string
): Promise<ReturnType<typeof cpServerToRow>> {
  const cp = await cpGetServer(orgId, serverId);
  if (!cp) throw new Error("Server does not belong to this organization.");
  const row = cpServerToRow(cp);
  // Keep the mirror fresh (name/status/ip can change) but never invent rows
  // the CP didn't confirm.
  await db
    .insert(schema.servers)
    .values(row)
    .onConflictDoUpdate({
      target: schema.servers.id,
      set: {
        name: row.name,
        type: row.type,
        source: row.source,
        provider: row.provider,
        region: row.region,
        status: row.status,
        agentVersion: row.agentVersion,
        ip: row.ip,
        cpu: row.cpu,
        memGb: row.memGb,
      },
    });
  return row;
}

type ServerRow = typeof s.servers.$inferSelect;

/** Map a control-plane server onto the local `servers` row shape the views
 *  render, so CP mode reuses the exact same components. */
export function cpServerToRow(cp: CpServer): ServerRow {
  const memTotalMb = cp.facts?.memTotalMb ?? 0;
  return {
    id: cp.id,
    orgId: cp.orgId,
    name: cp.name,
    type: cp.type,
    source: "byo",
    provider: cp.provider || "BYO",
    region: cp.region || "—",
    status: cp.status,
    agentVersion: cp.agentVersion,
    ip: cp.meshIp ?? "",
    cpu: cp.facts?.numCpu ?? 0,
    memGb: memTotalMb ? Math.round(memTotalMb / 1024) : 0,
    byoVpn: false,
    connectedAt: new Date(cp.createdAt),
  };
}

/** CP metrics → the chart's point shape (last 24h, oldest first upstream). */
export function cpMetricsToPoints(points: CpMetricPoint[]) {
  return points.map((p) => ({
    t: new Date(p.recordedAt).toLocaleTimeString("en-GB", {
      hour: "2-digit",
      minute: "2-digit",
    }),
    cpu: Math.round(p.cpuPct),
    mem: Math.round(p.memPct),
    disk: Math.round(p.diskPct),
  }));
}
