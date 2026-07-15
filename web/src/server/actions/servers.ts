"use server";

import { revalidatePath } from "next/cache";
import { eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireMembership } from "../active-org";
import { writeAudit } from "../audit";
import { cpEnabled, cpIssueBootstrapToken, cpPublicUrl } from "../cp";

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

function hashNum(str: string, mod: number) {
  let h = 5381;
  for (const c of str) h = ((h << 5) + h + c.charCodeAt(0)) >>> 0;
  return h % mod;
}

// Reported host capacity by type — simulated at agent check-in.
const SPEC: Record<string, { cpu: number; memGb: number }> = {
  general: { cpu: 4, memGb: 16 },
  database: { cpu: 8, memGb: 64 },
  storage: { cpu: 4, memGb: 16 },
  gpu: { cpu: 16, memGb: 128 },
};

export type ConnectServerResult =
  | {
      mode: "cp";
      /** Full sigmad invocation with the one-time bootstrap token embedded. */
      command: string;
      expiresAt: string;
    }
  | { mode: "sim"; id: string; bootstrapToken: string };

/** Register a BYO host.
 *
 *  CP mode (SIGMAHUB_CP_URL set): asks the control plane for a real one-time
 *  bootstrap token; the server record appears once the actual sigmad agent
 *  registers with it.
 *
 *  Demo mode: inserts a simulated `provisioning` row that the fake check-in
 *  button flips to running. */
export async function connectServer(input: {
  orgId: string;
  name: string;
  type: string; // general | database | storage | gpu
  provider: string;
  region: string;
  byoVpn?: boolean;
}): Promise<ConnectServerResult> {
  const { user, role } = await requireMembership(input.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Server name is required.");

  if (cpEnabled()) {
    const { token, expiresAt } = await cpIssueBootstrapToken(input.orgId, {
      name,
      type: input.type,
      provider: input.provider.trim(),
      region: input.region.trim(),
    }, { name: user.name, role });
    await writeAudit({
      orgId: input.orgId,
      actor: user.name,
      action: "Issued bootstrap token",
      target: name,
    });
    revalidatePath("/dashboard", "layout");
    return {
      mode: "cp",
      command: `sigmad --endpoint ${cpPublicUrl()} --bootstrap-token ${token} --name ${name}`,
      expiresAt,
    };
  }

  const id = rid("srv");
  await db.insert(s.servers).values({
    id,
    orgId: input.orgId,
    name,
    type: input.type,
    source: "byo",
    provider: input.provider.trim() || "BYO",
    region: input.region.trim() || "—",
    status: "provisioning",
    agentVersion: "",
    ip: "",
    cpu: 0,
    memGb: 0,
    byoVpn: Boolean(input.byoVpn),
  });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Connected server", target: name });
  revalidatePath("/dashboard", "layout");
  return { mode: "sim", id, bootstrapToken: `sk_boot_${id.slice(4, 12)}` };
}

/** Simulated agent check-in: flips provisioning → running and fills in the
 *  runtime details the agent would report (version, IP, CPU, memory). */
export async function agentCheckIn(input: { serverId: string }) {
  const [server] = await db
    .select()
    .from(s.servers)
    .where(eq(s.servers.id, input.serverId));
  if (!server) throw new Error("Server not found.");
  const { user } = await requireMembership(server.orgId);
  if (server.status !== "provisioning") return; // idempotent
  const spec = SPEC[server.type] ?? SPEC.general;
  await db
    .update(s.servers)
    .set({
      status: "running",
      agentVersion: "1.4.2",
      ip: `10.8.0.${20 + hashNum(server.id, 200)}`,
      cpu: spec.cpu,
      memGb: spec.memGb,
      connectedAt: new Date(),
    })
    .where(eq(s.servers.id, input.serverId));
  await writeAudit({ orgId: server.orgId, actor: user.name, action: "Agent checked in", target: server.name });
  revalidatePath("/dashboard", "layout");
}

/** Disconnect (delete) a server. Hosted resources are detached (serverId → null). */
export async function disconnectServer(input: { serverId: string }) {
  const [server] = await db
    .select()
    .from(s.servers)
    .where(eq(s.servers.id, input.serverId));
  if (!server) return;
  const { user } = await requireMembership(server.orgId);
  await db.delete(s.servers).where(eq(s.servers.id, input.serverId));
  await writeAudit({ orgId: server.orgId, actor: user.name, action: "Disconnected server", target: server.name });
  revalidatePath("/dashboard", "layout");
}
