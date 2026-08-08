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
import { createResourceBody } from "@/lib/deploy-spec";
import type { FailedRequirement, HostFacts } from "@/lib/server-compat";
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
  /** The agent's host description (SIGMA-201), reported at register and on
   *  every heartbeat. The shape lives in @/lib/server-compat because the demo
   *  gate is evaluated against exactly these fields — one declaration, so the
   *  two modes cannot disagree about what a host looks like. */
  facts: HostFacts | null;
  meshIp: string | null;
  /** Public ip:port — the connect wizard's host IP, refreshed by the agent's
   *  STUN probe. Distinct from the private 10.8.x.x meshIp (SIGMA-187). */
  endpoint: string | null;
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
  /** Configured hardening intent + edge role, so both are visible and editable
   *  after provisioning rather than write-once at connect time (SIGMA-178/179).
   *  `sshLocked` above is the agent's REPORTED posture; `keepPublicSsh` is what
   *  the operator asked for. */
  keepPublicSsh?: boolean;
  proxyRole?: boolean;
  /** Why status is 'incompatible' — one entry per requirement of this server's
   *  TYPE that its reported facts violate (SIGMA-203). Always an array from a
   *  current control plane; optional here so an older CP that predates the gate
   *  simply reads as "no reasons" rather than crashing the servers page. */
  incompatibleReasons?: FailedRequirement[];
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
  // Some CP endpoints (delete git connection / branch map / alert channel) reply
  // 204 No Content with an empty body. res.json() on an empty body throws
  // "Unexpected end of JSON input", which would make a SUCCEEDED delete report
  // failure and skip its audit write (SIGMA-118). Treat 204 / empty as void.
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (!text) return undefined as T;
  return JSON.parse(text) as T;
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
  input: {
    /** Optional since SIGMA-202: the connect form asks for the address and the
     *  type only, and registration names the server from the hostname the
     *  machine reports. A name here is the operator's and is never overwritten. */
    name?: string;
    type: string;
    provider: string;
    region: string;
    proxyRole: boolean;
    /** The connect form's host address — the server's initial public endpoint,
     *  and its placeholder name until the agent checks in (SIGMA-187/202). */
    hostIp?: string;
  },
  actor: CpActor
): Promise<{ serverId: string; token: string; expiresAt: string; bootstrapPubkey: string }> {
  return cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/servers/provision`, {
    method: "POST",
    body: JSON.stringify(input),
  }, { orgId, actor });
}

/** Re-issue the install command for an existing still-provisioning server
 *  (lost/expired token). The CP 409s once the server has registered. */
export async function cpReissueBootstrapToken(
  orgId: string,
  serverId: string,
  actor: CpActor
): Promise<{ serverId: string; token: string; expiresAt: string; bootstrapPubkey: string }> {
  return cpFetch(
    `/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/reissue-token`,
    { method: "POST", body: JSON.stringify({}) },
    { orgId, actor }
  );
}

/** Dashboard-driven agent upgrade: the CP renders an agent.update op until the
 *  agent's heartbeat reports the requested version. Project Admin+. */
export async function cpUpdateServerAgent(
  orgId: string,
  serverId: string,
  version: string,
  actor: CpActor
): Promise<void> {
  await cpFetch(
    `/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/agent-update`,
    { method: "POST", body: JSON.stringify({ version }) },
    { orgId, actor }
  );
}

/** Set a server's proxy/edge role. Opens 80/443 in the firewall and makes the
 *  reconciler render Traefik on this host — the precondition for any custom
 *  domain to route. Project Admin+ on the CP; audited (SIGMA-178). */
export async function cpSetProxyRole(
  orgId: string,
  serverId: string,
  proxy: boolean,
  actor: CpActor
): Promise<void> {
  await cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/proxy-role`, {
    method: "POST",
    body: JSON.stringify({ proxy }),
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
  /** PR-preview resource, torn down with its PR (SIGMA-194). */
  ephemeral?: boolean;
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

/** Edit an environment's production flag (SIGMA-190 — previously write-once at
 *  creation, silently inferred from the environment's name). */
export async function cpUpdateEnvironment(
  orgId: string,
  envId: string,
  input: { production: boolean },
  actor: CpActor
): Promise<CpEnvironment> {
  return cpFetch(`${org(orgId)}/environments/${encodeURIComponent(envId)}`, {
    method: "PATCH",
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
    /** Run this on one server. Exactly one of serverId / clusterId. */
    serverId?: string;
    /** Run this INSIDE a Kubernetes cluster; the scheduler picks the node, so
     *  there is no server to name. The store and the whole cluster render path
     *  have always accepted it — this client just never sent it, which is what
     *  made cluster deploys unreachable from the dashboard (SIGMA-200). */
    clusterId?: string;
    name: string;
    kind: string;
    spec?: Record<string, unknown>;
  },
  actor: CpActor
): Promise<CpResource> {
  return cpFetch(`${org(orgId)}/resources`, {
    method: "POST",
    body: JSON.stringify(createResourceBody(input)),
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

// P2-5b: point-in-time recovery. Provisions a fresh postgres resource recovered
// to targetTime (RFC3339). The CP validates the recoverable window server-side.
export async function cpRestoreDatabaseToTimestamp(
  orgId: string,
  resourceId: string,
  input: { name: string; environmentId: string; serverId: string; targetTime: string },
  actor: CpActor
): Promise<{ resource: CpResource; run: CpBackupRun }> {
  return cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}/restore-pitr`, {
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

/** One service of a detected Compose file.
 *
 *  A faithful mirror of `gitdetect.ComposeService`
 *  (cp/internal/gitdetect/compose.go) — every field that struct emits, none it
 *  doesn't. The CP has always returned this graph from /git/detect; the web type
 *  simply had no field for it, so `d.services` was parsed off the wire and
 *  thrown away, and a repo describing five services deployed as one container
 *  with nothing anywhere saying so (SIGMA-199). */
export type CpDetectedComposeService = {
  name: string;
  /** Build context (".", or a subdirectory) when the service builds from
   *  source; absent when it runs a prebuilt `image`. */
  build?: string;
  /** Dockerfile path, relative to the build context. */
  dockerfile?: string;
  /** Prebuilt image reference (present instead of `build`). */
  image?: string;
  /** Container ports the service exposes (the target side of a mapping). */
  ports?: number[];
  /** Fixed host ports the service binds. Two generations cannot hold the same
   *  host port, which is half of why `rollout` may be "recreate". */
  publishedPorts?: number[];
  /** Docker named volumes the service mounts — exclusive state, the other half
   *  of why `rollout` may be "recreate". */
  namedVolumes?: string[];
  dependsOn?: string[];
  /** Swap class the CP derived: "blue-green" (stateless) or "recreate" (holds
   *  an exclusive resource, so it goes down during the swap). */
  rollout: string;
};

/** Deploy config detected from a repo's root files — a wizard pre-fill. */
export type CpDetected = {
  hasDockerfile: boolean;
  hasCompose: boolean;
  dockerfilePath?: string;
  composePath?: string;
  ports: number[];
  env: string[];
  /** The Compose service graph; absent/empty for a plain Dockerfile app. This
   *  is what makes a multi-service deploy possible — see CpDetectedComposeService. */
  services?: CpDetectedComposeService[];
  healthCheck: CpHealthCheck;
  deployable: boolean;
  reason?: string;
  /** The provider-reported default branch — the wizard's auto-mapping target. */
  defaultBranch?: string;
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
  /** True when the CP auto-registered the push-to-deploy webhook on connect. */
  webhookRegistered?: boolean;
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
  /** Dedicated build server, when the mapping names one. */
  buildServerId?: string;
  createdAt: string;
  /** True when mapping enqueued the branch head as the first build. */
  initialDeploy?: boolean;
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
  /** queued | drained | no_targets. `no_targets` is the answer to "I pushed,
   *  why is nothing happening?" — the environment has no app resources yet,
   *  which used to be indistinguishable from a successful deploy. */
  status: string;
  /** How many deployments the push produced; `detail` says why when zero. */
  deploymentsCreated?: number;
  detail?: string;
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

// ── Billing (P2-4, Paddle) ──────────────────────────────────────────────────

export type CpBillingSummary = {
  configured: boolean;
  /** Connected SERVER count (what the fleet looks like). */
  connected: number;
  /** Weighted total the subscription actually bills. */
  units: number;
  billableUnits: number;
  /** Per-server-type explanation of `units`. */
  breakdown: { type: string; count: number; weight: number; units: number }[];
  freeTier: number;
  unitPrice: number;
  currency: string;
  amount: number;
  serverHoursThisMonth: number;
  month: string;
  subscription: {
    status: string; // none | active | past_due | canceled
    quantity: number;
    customerId?: string;
    subscriptionId?: string;
  };
};

export async function cpGetBilling(orgId: string): Promise<CpBillingSummary> {
  return cpFetch(`${org(orgId)}/billing`, undefined, { orgId });
}

export async function cpBillingCheckout(orgId: string, actor: CpActor): Promise<{ checkoutUrl: string }> {
  return cpFetch(`${org(orgId)}/billing/checkout`, {
    method: "POST",
    body: JSON.stringify({}),
  }, { orgId, actor });
}

export async function cpBillingPortal(orgId: string, actor: CpActor): Promise<{ portalUrl: string }> {
  return cpFetch(`${org(orgId)}/billing/portal`, {
    method: "POST",
    body: JSON.stringify({}),
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

// ── S3 buckets / keys / quotas (P2-1b, SIGMA-65) ────────────────────────────

export type CpBucket = {
  id: string;
  resourceId: string;
  name: string;
  quotaBytes: number;
  accessKey: string;
  status: string;
};

const bucketsBase = (orgId: string, resourceId: string) =>
  `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/buckets`;

export async function cpListBuckets(orgId: string, resourceId: string): Promise<CpBucket[]> {
  const res = await cpFetch<{ buckets: CpBucket[] }>(bucketsBase(orgId, resourceId), undefined, { orgId });
  return res.buckets ?? [];
}

export async function cpCreateBucket(
  orgId: string, resourceId: string, name: string, actor: CpActor
): Promise<CpBucket> {
  return cpFetch(bucketsBase(orgId, resourceId), {
    method: "POST",
    body: JSON.stringify({ name }),
  }, { orgId, actor });
}

export async function cpDeleteBucket(
  orgId: string, resourceId: string, bucket: string, actor: CpActor
): Promise<{ status: string }> {
  return cpFetch(`${bucketsBase(orgId, resourceId)}/${encodeURIComponent(bucket)}`, {
    method: "DELETE",
  }, { orgId, actor });
}

export async function cpSetBucketQuota(
  orgId: string, resourceId: string, bucket: string, quotaBytes: number, actor: CpActor
): Promise<{ status: string; quotaBytes: number }> {
  return cpFetch(`${bucketsBase(orgId, resourceId)}/${encodeURIComponent(bucket)}/quota`, {
    method: "PUT",
    body: JSON.stringify({ quotaBytes }),
  }, { orgId, actor });
}

export async function cpCreateBucketKey(
  orgId: string, resourceId: string, bucket: string, actor: CpActor
): Promise<{ accessKey: string }> {
  return cpFetch(`${bucketsBase(orgId, resourceId)}/${encodeURIComponent(bucket)}/key`, {
    method: "POST",
  }, { orgId, actor });
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
  input: {
    branch: string;
    environmentId: string;
    policy: "auto" | "manual";
    /** Build on a dedicated server instead of the deploy target. */
    buildServerId?: string;
  },
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
export async function cpListDeployRequests(orgId: string): Promise<CpDeployRequest[]> {
  const res = await cpFetch<{ deployRequests?: CpDeployRequest[] } | CpDeployRequest[]>(
    `${org(orgId)}/git/deploy-requests`,
    undefined,
    { orgId }
  );
  return Array.isArray(res) ? res : (res.deployRequests ?? []);
}

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
  trigger: string; // git | manual | rollback | config (re-ship after a secret/domain change)
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

/** The org-wide deploy feed: recent deployments (activity stream) plus the
 *  latest per resource, however old (SIGMA-161 — the dashboard's "Last deploy",
 *  "Version", "Active deploys" and activity feed read this via the mirror). */
export async function cpListOrgDeployments(
  orgId: string,
  limit = 50
): Promise<{ recent: CpDeployment[]; latest: CpDeployment[] }> {
  return cpFetch<{ recent: CpDeployment[]; latest: CpDeployment[] }>(
    `${org(orgId)}/deployments?limit=${limit}`, undefined, { orgId });
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
        meshIp: row.meshIp,
        cpu: row.cpu,
        memGb: row.memGb,
        // Same reason as cp-sync's upsert: these change over a host's life and
        // a mirror that never updates them is a stale answer, not an old one.
        facts: row.facts,
        incompatibleReasons: row.incompatibleReasons,
      },
    });
  return row;
}

type ServerRow = typeof s.servers.$inferSelect;

/** The host part of a WireGuard endpoint ("203.0.113.7:51820" → "203.0.113.7").
 *  Handles bracketed IPv6 ("[2001:db8::1]:51820"). */
export function endpointHost(endpoint: string | null | undefined): string {
  if (!endpoint) return "";
  const v6 = endpoint.match(/^\[(.+)\]:\d+$/);
  if (v6) return v6[1];
  const i = endpoint.lastIndexOf(":");
  return i > 0 && !endpoint.slice(i + 1).includes(":") ? endpoint.slice(0, i) : endpoint;
}

/** Map a control-plane server onto the local `servers` row shape the views
 *  render, so CP mode reuses the exact same components.
 *
 *  ip is the PUBLIC address (the wizard's host IP / the agent's STUN-probed
 *  endpoint); meshIp is the private 10.8.x.x WireGuard address. The mesh IP
 *  was previously mapped into `ip` and rendered under an "IP" header — a
 *  wrong answer for DNS, firewalls, or SSH (SIGMA-187). */
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
    ip: endpointHost(cp.endpoint),
    meshIp: cp.meshIp ?? "",
    cpu: cp.facts?.numCpu ?? 0,
    memGb: memTotalMb ? Math.round(memTotalMb / 1024) : 0,
    byoVpn: false,
    connectedAt: new Date(cp.createdAt),
    // The detected host, carried through so the detail page reads one shape in
    // both modes. Facts are what the connect form stopped asking the operator
    // for (SIGMA-202); the reasons are why the gate refused this host, and the
    // UI renders those sentences verbatim (SIGMA-203).
    facts: cp.facts ?? {},
    incompatibleReasons: cp.incompatibleReasons ?? [],
    nameAuto: false,
  };
}

/** Re-file a server under a different type. The control plane re-runs the
 *  compatibility gate against the facts it already has and answers with the
 *  server's NEW state — so a change that does not help says so immediately
 *  rather than after another heartbeat. Project Admin+; 409 when hosted
 *  resources could not run on the new type (SIGMA-203). */
export async function cpSetServerType(
  orgId: string,
  serverId: string,
  serverType: string,
  actor: CpActor
): Promise<CpServer> {
  return cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/type`, {
    method: "POST",
    body: JSON.stringify({ type: serverType }),
  }, { orgId, actor });
}

/** Rename a server. The connect form no longer asks for a name — registration
 *  takes the reported hostname — so this is where naming lives (SIGMA-202). */
export async function cpRenameServer(
  orgId: string,
  serverId: string,
  name: string,
  actor: CpActor
): Promise<CpServer> {
  return cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/rename`, {
    method: "POST",
    body: JSON.stringify({ name }),
  }, { orgId, actor });
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

// ── GitHub as an org-level integration ──────────────────────────────────────
// Connect the App once per organization, then SELECT repos per resource. The
// git connection push-to-deploy needs is derived by the CP, so nobody has to
// assemble one by hand for every repo.

export type CpGitHubInstallation = {
  installationId: string;
  orgId: string;
  accountLogin: string;
  accountType: string; // User | Organization
  createdBy: string;
  createdAt: string;
};

export type CpGitIntegration = {
  /** The App is configured on this control plane at all. */
  enabled: boolean;
  slug: string;
  /** At least one installation is connected to this org. */
  connected?: boolean;
  installations: CpGitHubInstallation[];
};

export type CpSelectableRepo = {
  fullName: string;
  private: boolean;
  defaultBranch: string;
  installationId?: string;
};

export type CpRepoList = {
  repos: CpSelectableRepo[];
  connected: boolean;
  /** The page cap stopped the walk — the list is partial and says so. */
  truncated?: boolean;
  /** Installations whose repos couldn't be read this time. */
  unavailable?: string[];
};

export async function cpGetGitIntegration(orgId: string): Promise<CpGitIntegration> {
  return cpFetch(`${org(orgId)}/git/integration`, undefined, { orgId });
}

export async function cpConnectGitIntegration(
  orgId: string,
  installationId: string,
  actor: CpActor
): Promise<CpGitHubInstallation> {
  return cpFetch(
    `${org(orgId)}/git/integration`,
    { method: "POST", body: JSON.stringify({ installationId }) },
    { orgId, actor }
  );
}

export async function cpDisconnectGitIntegration(
  orgId: string,
  installationId: string,
  actor: CpActor,
  force = false
): Promise<void> {
  await cpFetch(
    `${org(orgId)}/git/integration/${encodeURIComponent(installationId)}${force ? "?force=true" : ""}`,
    { method: "DELETE" },
    { orgId, actor }
  );
}

/** Every repo the org's installations can reach — the picker's data source. */
export async function cpListGitRepos(orgId: string): Promise<CpRepoList> {
  return cpFetch(`${org(orgId)}/git/repos`, undefined, { orgId });
}

/** Bind a repo to a project, deriving the git connection when there isn't one.
 *  Idempotent: selecting the same repo again returns the same connection. */
export async function cpSelectGitRepo(
  orgId: string,
  input: { projectId: string; repoFullName: string; installationId?: string },
  actor: CpActor
): Promise<CpGitConnection> {
  return cpFetch(
    `${org(orgId)}/git/repos/select`,
    { method: "POST", body: JSON.stringify(input) },
    { orgId, actor }
  );
}

// ── Compose placement (multi-service apps) ──────────────────────────────────

export type CpComposeService = {
  name: string;
  build?: string;
  dockerfile?: string;
  image?: string;
  ports?: number[];
  /** Why `rollout` is "recreate", when it is: a fixed host port cannot be held
   *  by two generations at once, and a named volume is exclusive state. */
  publishedPorts?: number[];
  namedVolumes?: string[];
  rollout?: string;
  dependsOn?: string[];
  /** Explicit placement; empty means the resource's own server. */
  serverId?: string;
  env?: Record<string, string>;
};

export type CpComposeServices = {
  services: CpComposeService[];
  /** Where services with no explicit placement run. */
  homeServerId: string;
};

export async function cpGetComposeServices(
  orgId: string,
  resourceId: string
): Promise<CpComposeServices> {
  return cpFetch(`${org(orgId)}/resources/${encodeURIComponent(resourceId)}/compose`, undefined, {
    orgId,
  });
}

export async function cpSetComposePlacements(
  orgId: string,
  resourceId: string,
  placements: { service: string; serverId: string; env?: Record<string, string> }[],
  actor: CpActor
): Promise<{ status: string; servers: string[] }> {
  return cpFetch(
    `${org(orgId)}/resources/${encodeURIComponent(resourceId)}/compose/placements`,
    { method: "PUT", body: JSON.stringify({ placements }) },
    { orgId, actor }
  );
}

// ── Kubernetes clusters ─────────────────────────────────────────────────────

export type CpClusterNode = {
  serverId: string;
  serverName: string;
  serverType: string;
  status: string;
  meshIp: string;
  role: string; // control-plane | worker
  joinedAt: string;
  /** What the node itself reported about k3s on it: pending | ready | error.
   *  Distinct from `status`, which only says whether the agent is checking in —
   *  an agent can be perfectly healthy on a host where k3s never installed. */
  nodeStatus: string;
  nodeMessage?: string;
  reportedAt?: string | null;
};

export type CpCluster = {
  id: string;
  orgId: string;
  environmentId: string;
  name: string;
  status: string; // provisioning | ready | degraded
  apiEndpoint: string;
  kubernetesVersion: string;
  createdBy: string;
  createdAt: string;
  nodes: CpClusterNode[];
};

export async function cpListClusters(
  orgId: string,
  environmentId?: string
): Promise<{ clusters: CpCluster[]; excludedKinds: string[] }> {
  const q = environmentId ? `?environmentId=${encodeURIComponent(environmentId)}` : "";
  return cpFetch(`${org(orgId)}/clusters${q}`, undefined, { orgId });
}

export async function cpCreateCluster(
  orgId: string,
  input: { environmentId: string; name: string; controlPlaneId: string },
  actor: CpActor
): Promise<CpCluster> {
  return cpFetch(
    `${org(orgId)}/clusters`,
    { method: "POST", body: JSON.stringify(input) },
    { orgId, actor, idempotencyKey: `cluster:${input.environmentId}:${input.name}` }
  );
}

export async function cpAddClusterNode(
  orgId: string,
  clusterId: string,
  serverId: string,
  actor: CpActor
): Promise<void> {
  await cpFetch(
    `${org(orgId)}/clusters/${encodeURIComponent(clusterId)}/nodes`,
    { method: "POST", body: JSON.stringify({ serverId }) },
    { orgId, actor }
  );
}

export async function cpRemoveClusterNode(
  orgId: string,
  clusterId: string,
  serverId: string,
  actor: CpActor
): Promise<void> {
  await cpFetch(
    `${org(orgId)}/clusters/${encodeURIComponent(clusterId)}/nodes/${encodeURIComponent(serverId)}`,
    { method: "DELETE" },
    { orgId, actor }
  );
}

export async function cpDeleteCluster(
  orgId: string,
  clusterId: string,
  actor: CpActor
): Promise<void> {
  await cpFetch(
    `${org(orgId)}/clusters/${encodeURIComponent(clusterId)}`,
    { method: "DELETE" },
    { orgId, actor }
  );
}

// ── Container image registry ────────────────────────────────────────────────

export type CpImageRegistry = {
  host: string;
  namespace: string;
  username: string;
  /** Whether a credential is stored. The value itself is never returned. */
  hasPassword: boolean;
  createdBy: string;
  updatedAt: string;
};

export async function cpGetRegistry(
  orgId: string
): Promise<{ configured: boolean; registry?: CpImageRegistry; repository?: string }> {
  return cpFetch(`${org(orgId)}/registry`, undefined, { orgId });
}

export async function cpSetRegistry(
  orgId: string,
  input: { host: string; namespace?: string; username?: string; password?: string },
  actor: CpActor
): Promise<{ registry: CpImageRegistry; repository: string }> {
  return cpFetch(
    `${org(orgId)}/registry`,
    { method: "PUT", body: JSON.stringify(input) },
    { orgId, actor }
  );
}

export async function cpDeleteRegistry(orgId: string, actor: CpActor): Promise<void> {
  await cpFetch(`${org(orgId)}/registry`, { method: "DELETE" }, { orgId, actor });
}

// ── DNS setup for custom domains ────────────────────────────────────────────

export type CpDNSRecord = { type: string; name: string; value: string; ttl: number };

export type CpDNSSetup = {
  domain: string;
  /** Apex domains must use an A record — a CNAME is illegal there. */
  apex: boolean;
  records: CpDNSRecord[];
  verified: boolean;
  /** What DNS currently answers, so a mismatch shows the actual wrong value. */
  observed?: string[];
  reason?: string;
  certStatus: string;
  checkedAt: string;
};

export async function cpDomainDNS(orgId: string, domainId: string): Promise<CpDNSSetup> {
  return cpFetch(`${org(orgId)}/domains/${encodeURIComponent(domainId)}/dns`, undefined, {
    orgId,
  });
}
