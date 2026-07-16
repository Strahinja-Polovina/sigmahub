"use server";

import { revalidatePath } from "next/cache";
import { eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireMembership, requireProjectAdmin, getActiveOrgId } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpIssueBootstrapToken,
  cpProvisionServer,
  cpSetHardening,
  cpPublicUrl,
  cpDeleteServer,
} from "../cp";

/** Release version the installer pins (installer requires an explicit tag). */
const AGENT_VERSION = process.env.SIGMAHUB_AGENT_VERSION ?? "latest";

/** The one-line, cosign-verified install command the wizard hands the operator. */
function installCommand(token: string): string {
  const ep = cpPublicUrl();
  return (
    `curl -fsSL ${ep}/install.sh | ` +
    `SIGMAHUB_ENDPOINT=${ep} SIGMAHUB_BOOTSTRAP_TOKEN=${token} ` +
    `SIGMAHUB_VERSION=${AGENT_VERSION} sudo -E bash`
  );
}

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

export type ProvisionResult =
  | {
      mode: "cp";
      serverId: string;
      /** curl|sh install command with the one-time bootstrap token embedded. */
      command: string;
      /** OpenSSH public key the operator adds to the host for one login. */
      bootstrapPubkey: string;
      expiresAt: string;
    }
  | { mode: "sim"; id: string };

/** SSH-onboarding wizard: pre-create the server (with type + proxy role +
 *  detected distro), mint the per-server bootstrap keypair, and return the
 *  cosign-verified install command. keepPublicSsh opts out of the SSH lockdown
 *  (a warned choice). Project Admin+ — provisioning is a privileged action. */
export async function provisionServer(input: {
  orgId: string;
  name: string;
  type: string;
  provider: string;
  region: string;
  proxyRole: boolean;
  distro?: string;
  keepPublicSsh?: boolean;
}): Promise<ProvisionResult> {
  const { user, role } = await requireProjectAdmin(input.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Server name is required.");

  if (!cpEnabled()) {
    // Demo mode: fall back to a simulated provisioning row.
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
      byoVpn: false,
    });
    await writeAudit({ orgId: input.orgId, actor: user.name, action: "Provisioned server (demo)", target: name });
    revalidatePath("/dashboard", "layout");
    return { mode: "sim", id };
  }

  const actor = { name: user.name, role };
  const res = await cpProvisionServer(
    input.orgId,
    {
      name,
      type: input.type,
      provider: input.provider.trim(),
      region: input.region.trim(),
      proxyRole: input.proxyRole,
      distro: input.distro,
    },
    actor
  );
  // The opt-out (keep public SSH) is a hardening-config change on the new server.
  if (input.keepPublicSsh) {
    await cpSetHardening(input.orgId, res.serverId, { keepPublicSsh: true, cisEnabled: true }, actor);
  }
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: input.keepPublicSsh ? "Provisioned server (public SSH kept)" : "Provisioned server",
    target: name,
  });
  revalidatePath("/dashboard", "layout");
  return {
    mode: "cp",
    serverId: res.serverId,
    command: installCommand(res.token),
    bootstrapPubkey: res.bootstrapPubkey,
    expiresAt: res.expiresAt,
  };
}

/** Toggle a server's hardening config from the dashboard (keep-public-SSH
 *  opt-out, CIS, extra inbound ports). Project Admin+. */
export async function setServerHardening(input: {
  orgId: string;
  serverId: string;
  keepPublicSsh: boolean;
  cisEnabled: boolean;
}) {
  const { user, role } = await requireProjectAdmin(input.orgId);
  if (cpEnabled()) {
    await cpSetHardening(input.orgId, input.serverId, {
      keepPublicSsh: input.keepPublicSsh,
      cisEnabled: input.cisEnabled,
    }, { name: user.name, role });
  }
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Updated server hardening",
    target: input.serverId,
  });
  revalidatePath("/dashboard", "layout");
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
  // In CP mode a connected-but-unattached server has NO local mirror row (only
  // attached servers get one), so the decommission must not be gated on it —
  // resolve the org from the row if present, else the active org.
  const orgId = server?.orgId ?? (await getActiveOrgId());
  if (!orgId) return;
  // Decommissioning is destructive — gate on Project Admin (P1-4), not bare
  // membership. In CP mode the control plane tombstones the server and revokes
  // its agent token (409 if resources are still bound — the thrown error aborts
  // before the local row is touched); the local mirror is then removed if present.
  const { user, role } = await requireProjectAdmin(orgId);
  if (cpEnabled()) {
    await cpDeleteServer(orgId, input.serverId, { name: user.name, role });
  }
  if (server) {
    await db.delete(s.servers).where(eq(s.servers.id, input.serverId));
  }
  await writeAudit({ orgId, actor: user.name, action: "Disconnected server", target: server?.name ?? input.serverId });
  revalidatePath("/dashboard", "layout");
}
