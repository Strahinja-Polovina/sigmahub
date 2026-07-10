import "server-only";

// Control-plane client (P0-7). When SIGMAHUB_CP_URL is set, the servers
// vertical reads/writes the real Go control plane instead of the simulated
// PGlite tables; with the flag unset the v1 demo path is untouched.

import type * as s from "./db/schema";

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

async function cpFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const token =
    process.env.SIGMAHUB_CP_SERVICE_TOKEN ?? "dev-service-token";
  const res = await fetch(`${cpBase()}${path}`, {
    ...init,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...init?.headers,
    },
    cache: "no-store",
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`Control plane ${res.status}: ${body.slice(0, 200)}`);
  }
  return res.json() as Promise<T>;
}

export async function cpListServers(orgId: string): Promise<CpServer[]> {
  const { servers } = await cpFetch<{ servers: CpServer[] }>(
    `/v1/orgs/${encodeURIComponent(orgId)}/servers`
  );
  return servers;
}

export async function cpGetServer(
  orgId: string,
  serverId: string
): Promise<CpServer | null> {
  try {
    return await cpFetch<CpServer>(
      `/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}`
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
    `/v1/orgs/${encodeURIComponent(orgId)}/servers/${encodeURIComponent(serverId)}/metrics`
  );
  return points;
}

export async function cpIssueBootstrapToken(
  orgId: string,
  input: { name: string; type: string; provider: string; region: string }
): Promise<{ token: string; expiresAt: string }> {
  return cpFetch(`/v1/orgs/${encodeURIComponent(orgId)}/bootstrap-tokens`, {
    method: "POST",
    body: JSON.stringify(input),
  });
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
