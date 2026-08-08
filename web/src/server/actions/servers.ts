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
  cpSetProxyRole,
  cpPublicUrl,
  cpDeleteServer,
  cpReissueBootstrapToken,
  cpUpdateServerAgent,
} from "../cp";

/** GitHub repo whose releases host install.sh + the pinned sigmad assets. */
const RELEASE_REPO = process.env.SIGMAHUB_RELEASE_REPO ?? "Strahinja-Polovina/sigmahub";

/** The released tag the installer pins. The installer needs a concrete release
 *  (its assets embed the version), so a missing value or "latest" is rejected
 *  loudly rather than rendered into a command that 404s at download time
 *  (SIGMA-157). */
function agentVersion(): string {
  const v = process.env.SIGMAHUB_AGENT_VERSION;
  if (!v || v === "latest") {
    throw new Error(
      "SIGMAHUB_AGENT_VERSION must be set to a released tag (e.g. v0.3.0) to onboard a server; " +
        `"latest" has no release asset. Set it in the control-plane deployment's environment.`
    );
  }
  return v;
}

/** The one-line, cosign-verified install command the wizard hands the operator.
 *  install.sh is fetched from the pinned GitHub release (the CP does not serve
 *  it), while SIGMAHUB_ENDPOINT points at the CP's public URL. */
function installCommand(token: string): string {
  const ep = cpPublicUrl();
  const version = agentVersion();
  const scriptUrl = `https://github.com/${RELEASE_REPO}/releases/download/${version}/install.sh`;
  return (
    `curl -fsSL ${scriptUrl} | ` +
    `SIGMAHUB_ENDPOINT=${ep} SIGMAHUB_BOOTSTRAP_TOKEN=${token} ` +
    `SIGMAHUB_VERSION=${version} sudo -E bash`
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
  // Registering a host issues a real one-time bootstrap token, so gate it at
  // Project Admin+ like provisionServer/disconnectServer — not bare membership
  // (SIGMA-82). The CP already enforces this; matching it here fails closed and
  // keeps the three server-lifecycle actions consistent.
  const { user, role } = await requireProjectAdmin(input.orgId);
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
  /** The wizard's "Host IP" — becomes the server's initial public endpoint
   *  instead of being collected and silently discarded (SIGMA-187). */
  hostIp?: string;
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
      hostIp: input.hostIp?.trim() || undefined,
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

/** Regenerate the install command for a server stuck in `provisioning` (lost
 *  or expired bootstrap token). The CP invalidates any outstanding token,
 *  mints a fresh keypair + token bound to the SAME server record, and 409s if
 *  the server already registered. Project Admin+. */
export async function reissueInstallCommand(input: { serverId: string }): Promise<{
  command: string;
  bootstrapPubkey: string;
  expiresAt: string;
}> {
  if (!cpEnabled()) throw new Error("Re-issuing an install command needs a control plane.");
  const [server] = await db
    .select()
    .from(s.servers)
    .where(eq(s.servers.id, input.serverId));
  // Same org resolution as disconnectServer: unattached CP servers have no
  // local mirror row.
  const orgId = server?.orgId ?? (await getActiveOrgId());
  if (!orgId) throw new Error("No active organization.");
  const { user, role } = await requireProjectAdmin(orgId);
  const res = await cpReissueBootstrapToken(orgId, input.serverId, { name: user.name, role });
  await writeAudit({
    orgId,
    actor: user.name,
    action: "Re-issued install command",
    target: server?.name ?? input.serverId,
  });
  revalidatePath("/dashboard", "layout");
  return {
    command: installCommand(res.token),
    bootstrapPubkey: res.bootstrapPubkey,
    expiresAt: res.expiresAt,
  };
}

/** Request a dashboard-driven agent upgrade to the platform's pinned release
 *  (SIGMAHUB_AGENT_VERSION). The agent downloads the cosign-verified release,
 *  swaps its binary and restarts — no operator SSH. Returns a readable result
 *  (thrown server-action errors are redacted in production). */
export async function updateServerAgent(input: { serverId: string }): Promise<
  { ok: true; version: string } | { ok: false; error: string }
> {
  try {
    if (!cpEnabled()) throw new Error("Agent updates need a control plane.");
    const version = agentVersion();
    const [server] = await db
      .select()
      .from(s.servers)
      .where(eq(s.servers.id, input.serverId));
    const orgId = server?.orgId ?? (await getActiveOrgId());
    if (!orgId) throw new Error("No active organization.");
    const { user, role } = await requireProjectAdmin(orgId);
    await cpUpdateServerAgent(orgId, input.serverId, version, { name: user.name, role });
    await writeAudit({
      orgId,
      actor: user.name,
      action: `Requested agent update to ${version}`,
      target: server?.name ?? input.serverId,
    });
    revalidatePath("/dashboard", "layout");
    return { ok: true, version };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : "Update request failed." };
  }
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

/** Set a server's proxy/edge role from the dashboard. Opening 80/443 and
 *  rendering Traefik is the precondition for a custom domain to route, so this
 *  must be changeable after provisioning — not only in the connect dialog
 *  (SIGMA-178). Project Admin+. */
export async function setServerProxyRole(input: {
  orgId: string;
  serverId: string;
  proxy: boolean;
}) {
  const { user, role } = await requireProjectAdmin(input.orgId);
  if (cpEnabled()) {
    await cpSetProxyRole(input.orgId, input.serverId, input.proxy, { name: user.name, role });
  }
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: input.proxy ? "Enabled proxy role" : "Disabled proxy role",
    target: input.serverId,
  });
  revalidatePath("/dashboard", "layout");
}

/** Simulated agent check-in: flips provisioning → running and fills in the
 *  runtime details the agent would report (version, IP, CPU, memory). Demo-only
 *  — in CP mode real agents report their own status, so fabricating mirror facts
 *  here would be fake state. The button is hidden in CP mode, but the action can
 *  be invoked directly, so guard it server-side too (SIGMA-82). */
export async function agentCheckIn(input: { serverId: string }) {
  if (cpEnabled()) return;
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
