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
  // P1-5 onboarding + hardening posture.
  distro?: string | null;
  ready?: boolean;
  meshPeerCount?: number;
  hardeningScore?: number | null;
  diskEncrypted?: boolean | null;
  sshLocked?: boolean | null;
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
): Promise<{ token: string; serverId: string; expiresAt: string }> {
  return cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/bootstrap-tokens`, {
    method: "POST",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

/** SSH onboarding: pre-create the server + mint a per-server bootstrap keypair.
 *  Returns the bound token + the OpenSSH public key to place on the host. */
export async function cpProvisionServer(
  orgId: string,
  input: { name: string; type: string; provider: string; region: string; proxyRole: boolean; distro?: string },
  actor: CpActor
): Promise<{ serverId: string; token: string; expiresAt: string; bootstrapPubkey: string }> {
  return cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/servers/provision`, {
    method: "POST",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

/** Update a server's hardening config (the keep-public-SSH opt-out, CIS, extra
 *  inbound ports). Project Admin+ on the CP; audited. */
export async function cpSetHardening(
  orgId: string,
  serverId: string,
  input: { keepPublicSsh: boolean; cisEnabled: boolean; extraPorts?: { port: number; proto: string }[] },
  actor: CpActor
): Promise<void> {
  await cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/hardening`, {
    method: "POST",
    body: JSON.stringify({
      keepPublicSsh: input.keepPublicSsh,
      cisEnabled: input.cisEnabled,
      extraPorts: input.extraPorts ?? [],
    }),
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

export async function cpListEnvironments(
  orgId: string,
  projectId: string
): Promise<CpEnvironment[]> {
  const { environments } = await cpFetch<{ environments: CpEnvironment[] }>(
    `${org(orgId)}/projects/${encodeURIComponent(projectId)}/environments`,
    undefined, { orgId });
  return environments;
}

export async function cpEnvServerIds(orgId: string, envId: string): Promise<string[]> {
  const { serverIds } = await cpFetch<{ serverIds: string[] }>(
    `${org(orgId)}/environments/${encodeURIComponent(envId)}/servers`,
    undefined, { orgId });
  return serverIds ?? [];
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

// Database resources (P1-10). Databases are mesh-only in v1: the CP publishes
// the engine port exclusively on the server's WireGuard mesh address.
export type CpBackupPolicy = {
  id: string;
  resourceId: string;
  schedule: string;
  keepDaily: number;
  keepWeekly: number;
  keepMonthly: number;
  targetId: string | null;
  enabled: boolean;
  pitrEnabled?: boolean;
};

export type CpDatabaseInfo = {
  resourceId: string;
  engine: string;
  image: string;
  host: string;
  port: number;
  database: string;
  username: string;
  meshOnly: boolean;
  backupPolicy?: CpBackupPolicy;
  // P2-5 PITR window: how far back a point-in-time restore can reach.
  lastWalAt?: string | null;
  lastWalSegment?: string;
};

export type CpDatabaseConnection = CpDatabaseInfo & {
  password: string;
  url: string;
};

/** Non-secret connection metadata + backup policy. Developer-visible. */
export async function cpGetDatabase(orgId: string, resourceId: string): Promise<CpDatabaseInfo | null> {
  try {
    return await cpFetch<CpDatabaseInfo>(
      `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/database`,
      undefined, { orgId }
    );
  } catch (err) {
    if (err instanceof Error && err.message.startsWith("Control plane 404")) {
      return null;
    }
    throw err;
  }
}

/** Audited credential reveal. The CP gates this at Project Admin+ (Developer
 *  tokens 403) and writes an audit row per reveal. */
export async function cpRevealDatabaseConnection(
  orgId: string,
  resourceId: string,
  actor: CpActor
): Promise<CpDatabaseConnection> {
  return cpFetch(
    `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/database/connection`,
    undefined, { orgId, actor }
  );
}

// Backups (P1-11): S3-compatible targets, per-resource policy, run history,
// the per-day verify feed and the fire-drill restore.
export type CpBackupTarget = {
  id: string;
  name: string;
  endpoint: string;
  bucket: string;
  region: string;
  forcePathStyle: boolean;
  accessKey: string;
  createdBy: string;
  createdAt: string;
};

export type CpBackupRun = {
  id: string;
  resourceId: string;
  kind: string; // backup | verify | restore
  status: string; // pending | running | success | failed
  snapshotId: string;
  dumpSha256: string;
  detail: string;
  restoreResourceId?: string | null;
  createdAt: string;
  finishedAt: string | null;
};

export type CpVerifyDay = { day: string; runs: number; failed: number; green: boolean };

export async function cpListBackupTargets(orgId: string): Promise<CpBackupTarget[]> {
  const { targets } = await cpFetch<{ targets: CpBackupTarget[] }>(
    `${org(orgId)}/backup-targets`, undefined, { orgId }
  );
  return targets;
}

export async function cpCreateBackupTarget(
  orgId: string,
  input: {
    name: string;
    endpoint: string;
    bucket: string;
    region: string;
    accessKey: string;
    secretKey: string;
  },
  actor: CpActor
): Promise<CpBackupTarget> {
  return cpFetch(`${org(orgId)}/backup-targets`, {
    method: "POST",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

export async function cpDeleteBackupTarget(orgId: string, targetId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/backup-targets/${encodeURIComponent(targetId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export async function cpUpdateBackupPolicy(
  orgId: string,
  resourceId: string,
  input: { targetId?: string | null; enabled?: boolean; keepDaily?: number; keepWeekly?: number; keepMonthly?: number; pitrEnabled?: boolean },
  actor: CpActor
): Promise<CpBackupPolicy> {
  return cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}/backup-policy`, {
    method: "PATCH",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

export async function cpListBackupRuns(orgId: string, resourceId: string, limit = 25): Promise<CpBackupRun[]> {
  const { runs } = await cpFetch<{ runs: CpBackupRun[] }>(
    `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/backup-runs?limit=${limit}`,
    undefined, { orgId }
  );
  return runs;
}

export async function cpVerifyDays(orgId: string, days = 30): Promise<CpVerifyDay[]> {
  const { days: out } = await cpFetch<{ days: CpVerifyDay[] }>(
    `${org(orgId)}/backups/verify-days?days=${days}`, undefined, { orgId }
  );
  return out;
}

// Telemetry (P1-13): tenant-isolated query proxies + the M1 beta-metrics feed.
export type CpTelemetryPoint = { t: string; cpu: number; mem: number; net: number };
export type CpLogLine = { t: string; level: "info" | "warn" | "error"; msg: string };
export type CpBetaMetrics = {
  deploys: { window: number; total: number; succeeded: number; rate: number };
  firstDeployAt: string | null;
  verifyStreakDays: number;
  connectedServers: number;
};

type PromMatrix = {
  data?: { result?: { metric: Record<string, string>; values: [number, string][] }[] };
};

function isNotConfigured(err: unknown): boolean {
  return err instanceof Error && err.message.includes("telemetry_not_configured");
}

/** Per-resource cpu/mem series from the pipeline. Returns null when the
 *  telemetry pipeline is not configured (the UI shows an explicit state —
 *  never fabricated data). */
export async function cpQueryResourceMetrics(
  orgId: string,
  resourceId: string
): Promise<CpTelemetryPoint[] | null> {
  const query = `{__name__=~"sigmahub_container_cpu_pct|sigmahub_container_mem_bytes",resource="${resourceId}"}`;
  let res: PromMatrix;
  try {
    res = await cpFetch<PromMatrix>(
      `${org(orgId)}/metrics/query?query=${encodeURIComponent(query)}&step=900`,
      undefined, { orgId }
    );
  } catch (err) {
    if (isNotConfigured(err)) return null;
    throw err;
  }
  const byTs = new Map<number, CpTelemetryPoint>();
  for (const series of res.data?.result ?? []) {
    const name = series.metric.__name__;
    for (const [ts, valS] of series.values) {
      const val = Number(valS);
      if (!Number.isFinite(val)) continue;
      let p = byTs.get(ts);
      if (!p) {
        const d = new Date(ts * 1000);
        p = {
          t: `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`,
          cpu: 0, mem: 0, net: 0,
        };
        byTs.set(ts, p);
      }
      if (name === "sigmahub_container_cpu_pct") p.cpu = Math.round(val * 10) / 10;
      // Memory renders in MiB so the chart shares a usable scale with CPU %.
      if (name === "sigmahub_container_mem_bytes") p.mem = Math.round(val / (1024 * 1024));
    }
  }
  return [...byTs.entries()].sort((a, b) => a[0] - b[0]).map(([, p]) => p);
}

type LokiResult = {
  data?: { result?: { stream: Record<string, string>; values: [string, string][] }[] };
};

/** Tenant-isolated log search. Filters are allowlisted parameters — the CP
 *  builds the LogQL selector server-side. Returns null when Loki is not
 *  configured. */
export async function cpQueryLogs(
  orgId: string,
  filter: { resourceId?: string; environmentId?: string; q?: string; limit?: number }
): Promise<CpLogLine[] | null> {
  const params = new URLSearchParams();
  if (filter.resourceId) params.set("resource", filter.resourceId);
  if (filter.environmentId) params.set("env", filter.environmentId);
  if (filter.q) params.set("q", filter.q);
  params.set("limit", String(filter.limit ?? 200));
  let res: LokiResult;
  try {
    res = await cpFetch<LokiResult>(
      `${org(orgId)}/logs/query?${params.toString()}`, undefined, { orgId }
    );
  } catch (err) {
    if (isNotConfigured(err)) return null;
    throw err;
  }
  const lines: { ms: number; line: CpLogLine }[] = [];
  for (const stream of res.data?.result ?? []) {
    const level: CpLogLine["level"] = stream.stream.stream === "stderr" ? "error" : "info";
    for (const [ns, text] of stream.values) {
      // Loki timestamps are ns-precision strings; millisecond precision is
      // plenty for display ordering and stays inside Number's safe range.
      const ms = Number(ns.slice(0, -6) || "0");
      lines.push({
        ms,
        line: {
          t: new Date(ms).toLocaleTimeString("en-GB", { hour12: false }),
          level,
          msg: text,
        },
      });
    }
  }
  lines.sort((a, b) => a.ms - b.ms);
  return lines.map((l) => l.line);
}

/** The M1 exit-criteria feed (org-scoped). */
export async function cpBetaMetrics(orgId: string): Promise<CpBetaMetrics | null> {
  try {
    return await cpFetch<CpBetaMetrics>(`${org(orgId)}/beta-metrics`, undefined, { orgId });
  } catch (err) {
    if (isNotConfigured(err)) return null;
    throw err;
  }
}

/** Fire-drill restore: provision a fresh database and load the source's latest
 *  snapshot into it. */
export async function cpRestoreDatabase(
  orgId: string,
  resourceId: string,
  input: { name: string; environmentId: string; serverId: string },
  actor: CpActor
): Promise<{ resource: CpResource; run: CpBackupRun }> {
  return cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}/restore`, {
    method: "POST",
    body: JSON.stringify(input),
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

// ── Secrets (P1-6) ──────────────────────────────────────────────────────────
// Metadata only ever crosses this boundary on list/create; a plaintext value is
// returned solely by the audited reveal, gated Project Admin+ on the CP.

export type CpSecret = {
  id: string;
  projectId: string;
  environmentId: string | null;
  name: string;
  envVar: boolean;
  createdBy: string;
};

/** List secret METADATA for a project. envId "" returns every secret in the
 *  project (project-scoped + all environments); envId set narrows to that
 *  environment's own secrets. Developer+ on the CP. */
export async function cpListSecrets(
  orgId: string,
  projectId: string,
  envId?: string
): Promise<CpSecret[]> {
  const qs = envId ? `?environmentId=${encodeURIComponent(envId)}` : "";
  const { secrets } = await cpFetch<{ secrets: CpSecret[] }>(
    `${org(orgId)}/projects/${encodeURIComponent(projectId)}/secrets${qs}`,
    undefined,
    { orgId }
  );
  return secrets;
}

export async function cpCreateSecret(
  orgId: string,
  projectId: string,
  input: { name: string; value: string; environmentId?: string; envVar: boolean },
  actor: CpActor
): Promise<CpSecret> {
  return cpFetch(
    `${org(orgId)}/projects/${encodeURIComponent(projectId)}/secrets`,
    {
      method: "POST",
      body: JSON.stringify({
        name: input.name,
        value: input.value,
        environmentId: input.environmentId ?? "",
        envVar: input.envVar,
      }),
    },
    { orgId, actor, idempotencyKey: crypto.randomUUID() }
  );
}

/** Reveal a secret's plaintext value — audited server-side, actor forwarded so
 *  the CP records who read it. Project Admin+ on the CP (Developer 403s). */
export async function cpRevealSecret(
  orgId: string,
  secretId: string,
  actor: CpActor
): Promise<string> {
  const { value } = await cpFetch<{ value: string }>(
    `${org(orgId)}/secrets/${encodeURIComponent(secretId)}/value`,
    undefined,
    { orgId, actor }
  );
  return value;
}

export async function cpDeleteSecret(orgId: string, secretId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/secrets/${encodeURIComponent(secretId)}`, {
    method: "DELETE",
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

// ── Git integration (P1-7) ───────────────────────────────────────────────────

/** Health probe detected from the repo, or a default TCP probe on the primary
 *  declared port when nothing is declared (the field P1-9's gate consumes). */
export type CpHealthCheck = {
  type: string; // "http" | "tcp"
  path?: string;
  port?: number;
  intervalSec: number;
  source: string; // "dockerfile" | "compose" | "default"
};

/** Deploy config detected from a repo's root files — a wizard pre-fill. */
export type CpDetected = {
  hasDockerfile: boolean;
  hasCompose: boolean;
  dockerfilePath?: string;
  composePath?: string;
  ports: number[];
  env: string[];
  healthCheck: CpHealthCheck;
  deployable: boolean;
  reason?: string;
};

export type CpGitConnection = {
  id: string;
  orgId: string;
  projectId: string;
  provider: string;
  installationId: string;
  repoFullName: string;
  createdBy: string;
  createdAt: string;
  previewsEnabled: boolean;
  previewServerId?: string;
};

// Previews (P1-12): per-PR ephemeral environments.
export type CpPreviewEnvironment = {
  id: string;
  connectionId: string;
  prNumber: number;
  environmentId: string;
  resourceId: string | null;
  branch: string;
  sha: string;
  status: string; // open | closed
  createdAt: string;
  closedAt: string | null;
};

export async function cpSetPreviews(
  orgId: string,
  connId: string,
  input: { enabled: boolean; serverId?: string },
  actor: CpActor
): Promise<void> {
  await cpFetch(`${org(orgId)}/git/connections/${encodeURIComponent(connId)}/previews`, {
    method: "PUT",
    body: JSON.stringify({ enabled: input.enabled, serverId: input.serverId ?? "" }),
  }, { orgId, actor });
}

export async function cpListPreviews(orgId: string, connId: string): Promise<CpPreviewEnvironment[]> {
  const { previews } = await cpFetch<{ previews: CpPreviewEnvironment[] }>(
    `${org(orgId)}/git/connections/${encodeURIComponent(connId)}/previews`,
    undefined, { orgId }
  );
  return previews;
}

export type CpBranchMap = {
  id: string;
  connectionId: string;
  branch: string;
  environmentId: string;
  policy: "auto" | "manual";
  lastRef?: string;
  lastSha?: string;
  lastPushedAt?: string;
  createdAt: string;
};

export type CpDeployRequest = {
  id: string;
  orgId: string;
  connectionId: string;
  environmentId?: string;
  kind: "deploy" | "pr_hook";
  ref: string;
  sha: string;
  branch?: string;
  status: string;
  createdAt: string;
};

/** Preview the deploy config sigmahub detects for a repo (persists nothing). */
export async function cpDetectRepo(
  orgId: string,
  input: { repoFullName: string; installationId?: string; token?: string },
  actor: CpActor
): Promise<CpDetected> {
  return cpFetch(`${org(orgId)}/git/detect`, {
    method: "POST",
    body: JSON.stringify({
      repoFullName: input.repoFullName,
      installationId: input.installationId ?? "",
      token: input.token ?? "",
    }),
  }, { orgId, actor });
}

/** Connect a repo to a project. The CP refuses (422) a repo that ships neither a
 *  Dockerfile nor a Compose file, surfaced here as a thrown error. */
export async function cpConnectRepo(
  orgId: string,
  input: { projectId: string; provider?: string; installationId?: string; repoFullName: string; token?: string },
  actor: CpActor
): Promise<CpGitConnection> {
  return cpFetch(`${org(orgId)}/git/connections`, {
    method: "POST",
    body: JSON.stringify({
      projectId: input.projectId,
      provider: input.provider ?? "github",
      installationId: input.installationId ?? "",
      repoFullName: input.repoFullName,
      token: input.token ?? "",
    }),
  }, { orgId, actor });
}

// ── S3 storage (P2-1) ───────────────────────────────────────────────────────

export type CpS3Info = {
  resourceId: string;
  engine: string;
  image: string;
  accessKey: string;
  host: string;
  port: number;
  meshOnly: boolean;
  endpoint: string;
};

export type CpS3Connection = CpS3Info & { secretKey: string };

export async function cpGetS3(orgId: string, resourceId: string): Promise<CpS3Info | null> {
  try {
    return await cpFetch<CpS3Info>(
      `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/s3`,
      undefined, { orgId });
  } catch (err) {
    if (err instanceof Error && err.message.startsWith("Control plane 404")) {
      return null;
    }
    throw err;
  }
}

export async function cpRevealS3Connection(
  orgId: string,
  resourceId: string,
  actor: CpActor
): Promise<CpS3Connection> {
  return cpFetch(
    `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/s3/connection`,
    undefined, { orgId, actor });
}

// ── Alerting (P2-6) ─────────────────────────────────────────────────────────

export type CpAlertChannel = {
  id: string;
  kind: string; // email | slack | telegram | webhook
  name: string;
  config: Record<string, unknown>;
  enabled: boolean;
  events: string[];
  lastOkAt?: string | null;
  lastError?: string;
  createdBy: string;
  createdAt: string;
};

export async function cpListAlertChannels(
  orgId: string
): Promise<{ channels: CpAlertChannel[]; events: string[] }> {
  return cpFetch(`${org(orgId)}/alert-channels`, undefined, { orgId });
}

export async function cpCreateAlertChannel(
  orgId: string,
  input: { kind: string; name: string; config?: Record<string, unknown>; secret?: string },
  actor: CpActor
): Promise<CpAlertChannel> {
  return cpFetch(`${org(orgId)}/alert-channels`, {
    method: "POST",
    body: JSON.stringify({
      kind: input.kind,
      name: input.name,
      config: input.config ?? {},
      secret: input.secret ?? "",
    }),
  }, { orgId, actor });
}

export async function cpDeleteAlertChannel(orgId: string, channelId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/alert-channels/${encodeURIComponent(channelId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export async function cpSetAlertRules(
  orgId: string,
  channelId: string,
  events: string[],
  actor: CpActor
): Promise<void> {
  await cpFetch(`${org(orgId)}/alert-channels/${encodeURIComponent(channelId)}/rules`, {
    method: "PUT",
    body: JSON.stringify({ events }),
  }, { orgId, actor });
}

/** Fires a synchronous test notification; throws with the transport error. */
export async function cpTestAlertChannel(orgId: string, channelId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/alert-channels/${encodeURIComponent(channelId)}/test`, {
    method: "POST",
    body: JSON.stringify({}),
  }, { orgId, actor });
}

/** GitHub App metadata (SIGMA-55): whether the CP can mint installation
 *  tokens, and the App slug for the installations/new link. */
export type CpGitAppInfo = { enabled: boolean; slug: string };

export async function cpGitAppInfo(orgId: string): Promise<CpGitAppInfo> {
  return cpFetch(`${org(orgId)}/git/app`, undefined, { orgId });
}

/** Link a GitHub App installation to an existing connection — from then on
 *  the CP clones and inspects with short-lived installation tokens. */
export async function cpLinkInstallation(
  orgId: string,
  connId: string,
  installationId: string,
  actor: CpActor
): Promise<void> {
  await cpFetch(
    `${org(orgId)}/git/connections/${encodeURIComponent(connId)}/installation`,
    { method: "POST", body: JSON.stringify({ installationId }) },
    { orgId, actor }
  );
}

export async function cpListGitConnections(orgId: string, projectId?: string): Promise<CpGitConnection[]> {
  const qs = projectId ? `?projectId=${encodeURIComponent(projectId)}` : "";
  const { connections } = await cpFetch<{ connections: CpGitConnection[] }>(
    `${org(orgId)}/git/connections${qs}`, undefined, { orgId });
  return connections;
}

export async function cpGetGitConnection(
  orgId: string,
  connId: string
): Promise<{ connection: CpGitConnection; branchMaps: CpBranchMap[] }> {
  return cpFetch(`${org(orgId)}/git/connections/${encodeURIComponent(connId)}`, undefined, { orgId });
}

export async function cpDisconnectRepo(orgId: string, connId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/git/connections/${encodeURIComponent(connId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export async function cpSetBranchMap(
  orgId: string,
  connId: string,
  input: { branch: string; environmentId: string; policy: "auto" | "manual" },
  actor: CpActor
): Promise<CpBranchMap> {
  return cpFetch(`${org(orgId)}/git/connections/${encodeURIComponent(connId)}/branches`, {
    method: "PUT",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

export async function cpDeleteBranchMap(orgId: string, mapId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/git/branch-maps/${encodeURIComponent(mapId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export async function cpPromoteBranch(orgId: string, mapId: string, actor: CpActor): Promise<CpDeployRequest> {
  return cpFetch(`${org(orgId)}/git/branch-maps/${encodeURIComponent(mapId)}/promote`, {
    method: "POST",
  }, { orgId, actor });
}

/** List a connection together with its branch routes in one hop per connection —
 *  the project view renders repo → branch→env tables. Resilient per connection:
 *  one failing detail fetch (e.g. a concurrent delete → 404) drops that repo,
 *  not the whole panel. */
export async function cpListGitConnectionsWithMaps(
  orgId: string,
  projectId: string
): Promise<{ connection: CpGitConnection; branchMaps: CpBranchMap[] }[]> {
  const connections = await cpListGitConnections(orgId, projectId);
  const settled = await Promise.all(
    connections.map(async (c) => {
      try {
        return await cpGetGitConnection(orgId, c.id);
      } catch {
        // Fall back to the list row with no branch maps rather than dropping it.
        return { connection: c, branchMaps: [] as CpBranchMap[] };
      }
    })
  );
  return settled;
}

// ── Custom domains (P1-8) ────────────────────────────────────────────────────

export type CpDomain = {
  id: string;
  orgId: string;
  resourceId: string;
  domain: string;
  challengeType: string;
  certStatus: string; // pending | issuing | issued | failed
  certSerial?: string;
  certExpiresAt?: string;
  lastError?: string;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
};

export async function cpListDomains(orgId: string, resourceId: string): Promise<CpDomain[]> {
  const { domains } = await cpFetch<{ domains: CpDomain[] }>(
    `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/domains`, undefined, { orgId });
  return domains;
}

export async function cpAttachDomain(
  orgId: string,
  resourceId: string,
  input: { domain: string; challengeType?: string },
  actor: CpActor
): Promise<CpDomain> {
  return cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}/domains`, {
    method: "POST",
    body: JSON.stringify({ domain: input.domain, challengeType: input.challengeType ?? "http" }),
  }, { orgId, actor });
}

export async function cpDetachDomain(orgId: string, domainId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/domains/${encodeURIComponent(domainId)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

// ── Deployments (P1-9) ───────────────────────────────────────────────────────

export type CpDeployment = {
  id: string;
  orgId: string;
  resourceId: string;
  environmentId?: string;
  serverId?: string;
  connectionId?: string;
  trigger: string; // git | manual | rollback
  gitRef?: string;
  gitSha?: string;
  imageDigest?: string;
  configHash?: string;
  status: string; // queued | building | deploying | success | failed | superseded | rolled_back
  detail?: string;
  rollbackOf?: string;
  buildSeconds?: number;
  durationSeconds?: number;
  createdBy?: string;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  // Compose multi-service deploy: how many services and each service's state.
  serviceCount?: number;
  serviceStatus?: Record<string, string>;
};

export type CpDeployLog = {
  id: number;
  stream: string;
  line: string;
  at: string;
};

export async function cpListDeployments(orgId: string, resourceId: string, limit = 25): Promise<CpDeployment[]> {
  const { deployments } = await cpFetch<{ deployments: CpDeployment[] }>(
    `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/deployments?limit=${limit}`, undefined, { orgId });
  return deployments;
}

export async function cpRollbackTargets(orgId: string, resourceId: string): Promise<CpDeployment[]> {
  const { targets } = await cpFetch<{ targets: CpDeployment[] }>(
    `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/rollback-targets`, undefined, { orgId });
  return targets;
}

export async function cpCreateRollback(
  orgId: string,
  resourceId: string,
  targetDeploymentId: string,
  actor: CpActor
): Promise<CpDeployment> {
  return cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}/rollback`, {
    method: "POST",
    body: JSON.stringify({ targetDeploymentId }),
  }, { orgId, actor });
}

/** Queue a manual redeploy (fresh clone→build→rollout of the last commit). */
export async function cpRedeploy(
  orgId: string,
  resourceId: string,
  actor: CpActor
): Promise<CpDeployment> {
  return cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}/deploy`, {
    method: "POST",
    body: JSON.stringify({}),
  }, { orgId, actor });
}

/** One page of a deployment's build/orchestration logs past a cursor (id). */
export async function cpDeployLogs(
  orgId: string,
  deploymentId: string,
  after = 0
): Promise<{ deployment: CpDeployment; logs: CpDeployLog[]; nextCursor: number; done: boolean }> {
  return cpFetch(
    `${org(orgId)}/deployments/${encodeURIComponent(deploymentId)}/logs?after=${after}`,
    undefined, { orgId });
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
