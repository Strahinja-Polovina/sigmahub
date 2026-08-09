"use server";

import { revalidatePath } from "next/cache";
import { eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireMembership, requireProjectAdmin, getActiveOrgId } from "../active-org";
import { writeAudit } from "../audit";
import type { ServerType } from "@/lib/server-catalog.generated";
import { DECOMMISSION_TIMEOUT_MS, forceReason } from "@/lib/decommission";
import {
  checkServerCompatibility,
  nextServerStatus,
  statusAfterTypeChange,
  SERVER_STATUS,
  type FailedRequirement,
  type HostFacts,
} from "@/lib/server-compat";
import {
  cpEnabled,
  cpIssueBootstrapToken,
  cpProvisionServer,
  cpSetHardening,
  cpSetProxyRole,
  cpPublicUrl,
  cpDecommissionServer,
  cpDeleteServer,
  cpGetServer,
  boundResourcesOf,
  cpReissueBootstrapToken,
  cpRenameServer,
  cpSetServerType,
  cpUpdateServerAgent,
  type CpInstallerRelease,
} from "../cp";

/** The one-line, cosign-verified install command the wizard hands the operator.
 *
 *  Every URL in it is the control plane's. It serves install.sh, and
 *  SIGMAHUB_DOWNLOAD_BASE sends the five asset downloads the script then makes
 *  (the sigmad archive, checksums.txt and its .sig/.pem, sigmad.service) back
 *  through it as well — agent/packaging/install.sh honours that variable for
 *  all of them, which is why this is a URL to hand over rather than a fetching
 *  strategy to reimplement.
 *
 *  It used to point curl straight at github.com, and that worked only while the
 *  release repository was PUBLIC: an operator with a private one watched all
 *  five assets 404 and could not onboard a single server. The control plane
 *  authenticates to GitHub with its own credential, so the fix costs the
 *  command nothing — no token appears in a string that gets pasted into a
 *  terminal, screenshotted into a ticket and left in shell history.
 *
 *  What the proxy explicitly does NOT do is vouch for the bytes, and it was
 *  never asked to. install.sh cosign-verifies checksums.txt against the release
 *  workflow's keyless OIDC identity before executing anything, and verifies the
 *  archive and the unit against that checksums.txt; the signature is the
 *  authenticity and the transport is only reachability. A control plane that
 *  served a tampered asset would fail that verification exactly as a tampered
 *  github.com would. */
function installCommand(token: string, release: CpInstallerRelease): string {
  const ep = cpPublicUrl();
  // install.sh is the ONE artifact cosign does not cover, because install.sh is
  // what runs cosign. Its integrity has always rested on TLS: the old command
  // hard-coded https://github.com/…, and moving the fetch to the control plane
  // moved that trust to TLS against the control plane without moving the
  // requirement with it. The deployment guide shipped
  // SIGMAHUB_CP_PUBLIC_URL=http://your-host:8080 and the control plane
  // terminates no TLS of its own, so the documented default piped plaintext
  // into `sudo bash` — an on-path attacker goes from reading a bootstrap token
  // to root on every host being onboarded.
  //
  // Refused here rather than warned about, and for the same reason the missing
  // release below is: the alternative is a command that looks fine, works, and
  // is a remote code execution primitive on the network between the operator
  // and their control plane.
  if (!ep.startsWith("https://")) {
    throw new Error(
      `The install command pipes a script from ${ep || "the control plane"} straight into sudo bash, so it ` +
        "must be fetched over TLS. Set SIGMAHUB_CP_PUBLIC_URL to an https:// URL — put the control plane " +
        "behind the TLS terminator you already run, and use its address here."
    );
  }
  // The version comes back with the token, from the control plane that is going
  // to serve every URL in this line. The dashboard used to read its own
  // SIGMAHUB_AGENT_VERSION here, which meant the /dl/{version} paths and the
  // release GET /install.sh actually serves were two settings that happened to
  // be equal in a working deployment — and in a mismatched one the operator got
  // one release's installer pointed at another release's assets, or a 404 from
  // a control plane refusing a version it does not serve. Neither half was
  // wrong; there were simply two of them.
  const version = release.agentVersion;
  if (!version) {
    // The control plane's own sentence, not a paraphrase: it names the setting
    // (CP_AGENT_VERSION / CP_RELEASE_REPO) on the machine that has to change.
    throw new Error(
      release.agentVersionError ??
        "The control plane is not pinned to a released agent version, so there is no install command to render. Set CP_AGENT_VERSION on the control plane to a released tag such as v0.3.0."
    );
  }
  return (
    `curl -fsSL ${ep}/install.sh | ` +
    `SIGMAHUB_ENDPOINT=${ep} SIGMAHUB_BOOTSTRAP_TOKEN=${token} ` +
    `SIGMAHUB_VERSION=${version} SIGMAHUB_DOWNLOAD_BASE=${ep}/dl/${version} sudo -E bash`
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

// Reported host capacity by type — simulated at agent check-in (demo mode only;
// in CP mode the real agent reports its own facts).
//
// Keyed on the generated ServerType union, so adding a server type to the CP
// catalog fails `tsc` here rather than silently giving the new type a general
// server's shape. It used to name four types and fall through for the other
// three, which is how a demo VPS reported 16 GB of RAM.
//
// diskGb and gpu were added with SIGMA-201/203: the demo host has to describe
// itself the way a real one does, because the compatibility gate reads those
// facts and demo mode has to be able to show its verdict.
const SPEC: Record<ServerType, { cpu: number; memGb: number; diskGb: number; gpu?: boolean }> = {
  general: { cpu: 4, memGb: 16, diskGb: 480 },
  vps: { cpu: 2, memGb: 8, diskGb: 160 },
  database: { cpu: 8, memGb: 64, diskGb: 2000 },
  storage: { cpu: 4, memGb: 16, diskGb: 8000 },
  gpu: { cpu: 16, memGb: 128, diskGb: 2000, gpu: true },
  k8s: { cpu: 8, memGb: 32, diskGb: 480 },
  build: { cpu: 8, memGb: 32, diskGb: 960 },
};

/** How a demo check-in describes the host (SIGMA-215 parity).
 *
 *  "matching" is the machine the operator meant to connect — a host that meets
 *  the type's requirements, which is what a demo should show most of the time.
 *
 *  "generic" is an ordinary 4-vCPU / 60 GB box with no accelerator: exactly
 *  what someone actually plugs in when they pick GPU or Storage by mistake. It
 *  is the only way to reach the `incompatible` state without owning the wrong
 *  hardware, and it recovers the moment the server is re-filed as a type the
 *  box does satisfy — which is the whole flow SIGMA-203 added. */
export type DemoHostShape = "matching" | "generic";

const GB = 1_000_000_000;

function simulatedFacts(serverId: string, type: string, shape: DemoHostShape): HostFacts {
  const spec = SPEC[type as ServerType] ?? SPEC.general;
  const n = 20 + hashNum(serverId, 200);
  if (shape === "generic") {
    return {
      hostname: `sigma-host-${n}`,
      os: "linux",
      arch: "amd64",
      numCpu: 4,
      memTotalMb: 16 * 1024,
      distro: "ubuntu-24.04",
      distroName: "Ubuntu 24.04.1 LTS",
      diskTotalBytes: 60 * GB,
      diskFreeBytes: 41 * GB,
      diskPath: "/",
      // Present and empty: the host was asked and has no accelerator. Absent
      // would mean "nobody looked", which the gate treats as unknown — and a
      // demo that could not distinguish the two could not demonstrate it.
      gpu: { vendor: "", count: 0 },
      dockerAvailable: true,
      dockerVersion: "27.3.1",
    };
  }
  return {
    hostname: `sigma-${type}-${n}`,
    os: "linux",
    arch: "amd64",
    numCpu: spec.cpu,
    memTotalMb: spec.memGb * 1024,
    distro: "ubuntu-24.04",
    distroName: "Ubuntu 24.04.1 LTS",
    diskTotalBytes: spec.diskGb * GB,
    diskFreeBytes: Math.round(spec.diskGb * 0.7) * GB,
    diskPath: "/var/lib/sigmad",
    gpu: spec.gpu
      ? {
          vendor: "nvidia",
          model: "NVIDIA L40S",
          count: 2,
          vramBytesPerGpu: 48_301_604_864,
          vramBytesTotal: 96_603_209_728,
          driverVersion: "550.54.15",
          cards: [
            { index: 0, model: "NVIDIA L40S", vramBytes: 48_301_604_864 },
            { index: 1, model: "NVIDIA L40S", vramBytes: 48_301_604_864 },
          ],
        }
      : { vendor: "", count: 0 },
    dockerAvailable: true,
    dockerVersion: "27.3.1",
  };
}

export type ConnectServerResult =
  | {
      mode: "cp";
      /** Full sigmad invocation with the one-time bootstrap token embedded. */
      command: string;
      expiresAt: string;
    }
  | { mode: "sim"; id: string; bootstrapToken: string };

/** The placeholder a server carries until its agent reports a hostname. The
 *  address is the only handle the operator has on the box at that moment, so a
 *  row named for it is identifiable in a list; "server" is the last resort for
 *  the manual/NAT path, which has no address either (SIGMA-202). */
function placeholderName(hostIp: string): string {
  return hostIp.trim() || "server";
}

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
  type: string; // a ServerType; validated against the CP catalog server-side
  hostIp: string;
  provider?: string;
  region?: string;
  byoVpn?: boolean;
}): Promise<ConnectServerResult> {
  // Registering a host issues a real one-time bootstrap token, so gate it at
  // Project Admin+ like provisionServer/disconnectServer — not bare membership
  // (SIGMA-82). The CP already enforces this; matching it here fails closed and
  // keeps the three server-lifecycle actions consistent.
  const { user, role } = await requireProjectAdmin(input.orgId);
  // The connect form asks for two things, and the address is one of them —
  // it is where the install command is going to run.
  const hostIp = input.hostIp.trim();
  if (!hostIp) throw new Error("The host's IP address or hostname is required.");
  const name = placeholderName(hostIp);

  if (cpEnabled()) {
    const { token, expiresAt } = await cpIssueBootstrapToken(input.orgId, {
      // No name: the control plane pre-creates the row under a placeholder and
      // replaces it with the hostname the agent reports, the same rule the SSH
      // path follows (SIGMA-202).
      name: "",
      type: input.type,
      provider: input.provider?.trim() ?? "",
      region: input.region?.trim() ?? "",
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
      command: `sigmad --endpoint ${cpPublicUrl()} --bootstrap-token ${token}`,
      expiresAt,
    };
  }

  const id = rid("srv");
  await db.insert(s.servers).values({
    id,
    orgId: input.orgId,
    name,
    // The name came from the address, not from the operator: the simulated
    // check-in replaces it with the reported hostname, exactly as registration
    // does in CP mode.
    nameAuto: true,
    type: input.type,
    source: "byo",
    provider: input.provider?.trim() || "BYO",
    region: input.region?.trim() || "—",
    status: SERVER_STATUS.provisioning,
    agentVersion: "",
    ip: hostIp,
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

/** The connect flow: pre-create the server from the two things the operator
 *  actually knows — the host's address and what they want it to be — mint the
 *  per-server bootstrap keypair, and return the cosign-verified install command
 *  immediately, so the dialog can show it while it waits for the agent.
 *
 *  It no longer takes a name or a distro. The machine reports its hostname and
 *  reads its own /etc/os-release; asking the operator to guess either before
 *  they have logged in is the inversion SIGMA-202 removes. Provider and region
 *  stay as optional metadata. keepPublicSsh opts out of the SSH lockdown (a
 *  warned choice). Project Admin+ — provisioning is a privileged action. */
export async function provisionServer(input: {
  orgId: string;
  type: string;
  /** The host's public address. Becomes the server's initial endpoint and its
   *  placeholder name until the agent checks in (SIGMA-187/202). */
  hostIp: string;
  provider?: string;
  region?: string;
  proxyRole?: boolean;
  keepPublicSsh?: boolean;
}): Promise<ProvisionResult> {
  const { user, role } = await requireProjectAdmin(input.orgId);
  const hostIp = input.hostIp.trim();
  if (!hostIp) throw new Error("The host's IP address or hostname is required.");
  const name = placeholderName(hostIp);

  if (!cpEnabled()) {
    // Demo mode: fall back to a simulated provisioning row.
    const id = rid("srv");
    await db.insert(s.servers).values({
      id,
      orgId: input.orgId,
      name,
      nameAuto: true,
      type: input.type,
      source: "byo",
      provider: input.provider?.trim() || "BYO",
      region: input.region?.trim() || "—",
      status: SERVER_STATUS.provisioning,
      agentVersion: "",
      ip: hostIp,
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
      // No name: the control plane pre-creates the row under the address and
      // replaces it with the hostname the agent reports.
      type: input.type,
      provider: input.provider?.trim() ?? "",
      region: input.region?.trim() ?? "",
      proxyRole: Boolean(input.proxyRole),
      hostIp,
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
    command: installCommand(res.token, res),
    bootstrapPubkey: res.bootstrapPubkey,
    expiresAt: res.expiresAt,
  };
}

/** What the connect dialog is waiting for: the live state of the row the
 *  install command is going to fill in.
 *
 *  The dialog polls this while it shows the command, so "waiting for agent…"
 *  turns into the machine's own name, or into the gate's verdict, without the
 *  operator refreshing anything. Read-only and member-visible: it says nothing
 *  a member cannot already see on the servers page. */
export type ServerConnectionState = {
  id: string;
  name: string;
  type: string;
  status: string;
  /** Empty until the agent registers. */
  distro: string;
  arch: string;
  cpu: number;
  memGb: number;
  diskTotalBytes: number;
  gpu: string;
  /** Rendered verbatim when status is `incompatible` (SIGMA-203). */
  incompatibleReasons: FailedRequirement[];
};

function connectionStateFromFacts(row: {
  id: string;
  name: string;
  type: string;
  status: string;
  cpu: number;
  memGb: number;
  facts: HostFacts | null;
  incompatibleReasons: FailedRequirement[];
}): ServerConnectionState {
  const f = row.facts ?? {};
  const gpu = f.gpu;
  return {
    id: row.id,
    name: row.name,
    type: row.type,
    status: row.status,
    distro: f.distroName || f.distro || "",
    arch: f.arch ?? "",
    cpu: f.numCpu ?? row.cpu,
    memGb: f.memTotalMb ? Math.round(f.memTotalMb / 1024) : row.memGb,
    diskTotalBytes: f.diskTotalBytes ?? 0,
    gpu: gpu && (gpu.count ?? 0) > 0 ? `${gpu.count} × ${gpu.model || gpu.vendor}` : "",
    incompatibleReasons: row.incompatibleReasons ?? [],
  };
}

export async function serverConnectionState(input: {
  orgId: string;
  serverId: string;
}): Promise<ServerConnectionState | null> {
  await requireMembership(input.orgId);
  if (cpEnabled()) {
    const cp = await cpGetServer(input.orgId, input.serverId);
    if (!cp) return null;
    return connectionStateFromFacts({
      id: cp.id,
      name: cp.name,
      type: cp.type,
      status: cp.status,
      cpu: 0,
      memGb: 0,
      facts: cp.facts,
      incompatibleReasons: cp.incompatibleReasons ?? [],
    });
  }
  const [row] = await db.select().from(s.servers).where(eq(s.servers.id, input.serverId));
  if (!row || row.orgId !== input.orgId) return null;
  return connectionStateFromFacts(row);
}

/** Exit 1 from an incompatible enrollment: re-file the server under a type it
 *  can actually be. The gate is re-run against the facts already on record, so
 *  the answer is immediate — including when the new type does not help either,
 *  which is reported rather than hidden behind an optimistic toast.
 *
 *  Returns a readable result instead of throwing: this is rendered inside a
 *  panel that is already explaining a failure, and a redacted server-action
 *  error there would replace a real explanation with "An error occurred". */
export async function changeServerType(input: {
  orgId: string;
  serverId: string;
  type: string;
}): Promise<{ ok: true; state: ServerConnectionState | null } | { ok: false; error: string }> {
  try {
    const { user, role } = await requireProjectAdmin(input.orgId);
    if (cpEnabled()) {
      const cp = await cpSetServerType(input.orgId, input.serverId, input.type, { name: user.name, role });
      await writeAudit({
        orgId: input.orgId,
        actor: user.name,
        action: `Changed server type to ${input.type}`,
        target: cp.name,
      });
      revalidatePath("/dashboard", "layout");
      return {
        ok: true,
        state: connectionStateFromFacts({
          id: cp.id, name: cp.name, type: cp.type, status: cp.status, cpu: 0, memGb: 0,
          facts: cp.facts, incompatibleReasons: cp.incompatibleReasons ?? [],
        }),
      };
    }

    const [row] = await db.select().from(s.servers).where(eq(s.servers.id, input.serverId));
    if (!row || row.orgId !== input.orgId) throw new Error("Server not found.");
    // Demo mode re-runs the same gate the control plane would, against the
    // facts the simulated check-in reported — so the exit behaves identically
    // in both modes rather than always "succeeding" here.
    const reasons = checkServerCompatibility(input.type, row.facts);
    // connectedAt is the only liveness stamp the demo schema carries; a demo
    // host is "seen" for as long as it is connected. What matters either way is
    // the half this fixes: an unreachable row is never revived by a re-file.
    const status = statusAfterTypeChange(row.status, reasons, row.connectedAt);
    await db
      .update(s.servers)
      .set({ type: input.type, status, incompatibleReasons: reasons })
      .where(eq(s.servers.id, input.serverId));
    await writeAudit({
      orgId: input.orgId,
      actor: user.name,
      action: `Changed server type to ${input.type}`,
      target: row.name,
    });
    revalidatePath("/dashboard", "layout");
    return {
      ok: true,
      state: connectionStateFromFacts({ ...row, type: input.type, status, incompatibleReasons: reasons }),
    };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : "Couldn’t change the server type." };
  }
}

/** Name a server. The connect form stopped asking (the machine reports its
 *  hostname), so this is where naming lives — and renaming clears the
 *  machine-assigned flag for good (SIGMA-202). */
export async function renameServer(input: {
  orgId: string;
  serverId: string;
  name: string;
}): Promise<{ ok: true; name: string } | { ok: false; error: string }> {
  try {
    const { user, role } = await requireProjectAdmin(input.orgId);
    const name = input.name.trim();
    if (!name) throw new Error("A server name is required.");
    if (cpEnabled()) {
      const cp = await cpRenameServer(input.orgId, input.serverId, name, { name: user.name, role });
      await writeAudit({ orgId: input.orgId, actor: user.name, action: "Renamed server", target: name });
      revalidatePath("/dashboard", "layout");
      return { ok: true, name: cp.name };
    }
    const [row] = await db.select().from(s.servers).where(eq(s.servers.id, input.serverId));
    if (!row || row.orgId !== input.orgId) throw new Error("Server not found.");
    await db
      .update(s.servers)
      .set({ name, nameAuto: false })
      .where(eq(s.servers.id, input.serverId));
    await writeAudit({ orgId: input.orgId, actor: user.name, action: "Renamed server", target: name });
    revalidatePath("/dashboard", "layout");
    return { ok: true, name };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : "Couldn’t rename the server." };
  }
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
    command: installCommand(res.token, res),
    bootstrapPubkey: res.bootstrapPubkey,
    expiresAt: res.expiresAt,
  };
}

/** Request a dashboard-driven agent upgrade to the platform's pinned release.
 *  The agent downloads the cosign-verified release, swaps its binary and
 *  restarts — no operator SSH. Returns a readable result (thrown server-action
 *  errors are redacted in production).
 *
 *  Which release that is, is the control plane's to say and not this process's:
 *  the agent fetches it through the control plane's /dl route, which serves one
 *  version. So no version is sent, and the one it applied comes back. */
export async function updateServerAgent(input: { serverId: string }): Promise<
  { ok: true; version: string } | { ok: false; error: string }
> {
  try {
    if (!cpEnabled()) throw new Error("Agent updates need a control plane.");
    const [server] = await db
      .select()
      .from(s.servers)
      .where(eq(s.servers.id, input.serverId));
    const orgId = server?.orgId ?? (await getActiveOrgId());
    if (!orgId) throw new Error("No active organization.");
    const { user, role } = await requireProjectAdmin(orgId);
    const { version } = await cpUpdateServerAgent(orgId, input.serverId, { name: user.name, role });
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

/** Simulated agent check-in: reports the host's facts, names the server from
 *  its hostname, runs the compatibility gate over the result and lands the row
 *  in whatever status that produces. Demo-only — in CP mode real agents report
 *  their own status, so fabricating mirror facts here would be fake state. The
 *  button is hidden in CP mode, but the action can be invoked directly, so
 *  guard it server-side too (SIGMA-82).
 *
 *  `shape` is what the pretend machine turns out to be. "generic" is an
 *  ordinary box with no accelerator and a small disk — the machine someone
 *  actually plugs in when they picked GPU or Storage by mistake — and it is the
 *  only way to see the incompatible state, and the exits out of it, without
 *  owning the wrong hardware (SIGMA-203/215). It is not a re-check either: it
 *  is what the agent found this time, so running it after a "matching" check-in
 *  is the card being pulled, and vice versa is the driver being installed. */
export async function agentCheckIn(input: { serverId: string; shape?: DemoHostShape }) {
  if (cpEnabled()) return;
  const [server] = await db
    .select()
    .from(s.servers)
    .where(eq(s.servers.id, input.serverId));
  if (!server) throw new Error("Server not found.");
  const { user } = await requireMembership(server.orgId);
  if (server.status === SERVER_STATUS.running && !input.shape) return; // idempotent

  const shape = input.shape ?? "matching";
  const facts = simulatedFacts(server.id, server.type, shape);
  const reasons = checkServerCompatibility(server.type, facts);
  const status = nextServerStatus(server.status, reasons, true);
  await db
    .update(s.servers)
    .set({
      status,
      agentVersion: "1.4.2",
      // The machine's own name replaces the address we filed it under, once.
      name: server.nameAuto && facts.hostname ? facts.hostname : server.name,
      nameAuto: server.nameAuto && facts.hostname ? false : server.nameAuto,
      meshIp: `10.8.0.${20 + hashNum(server.id, 200)}`,
      facts,
      incompatibleReasons: reasons,
      cpu: facts.numCpu ?? 0,
      memGb: facts.memTotalMb ? Math.round(facts.memTotalMb / 1024) : 0,
      connectedAt: new Date(),
    })
    .where(eq(s.servers.id, input.serverId));
  await writeAudit({
    orgId: server.orgId,
    actor: user.name,
    action: reasons.length > 0 ? `Server incompatible — ${reasons[0].reason}` : "Agent checked in",
    target: facts.hostname ?? server.name,
  });
  revalidatePath("/dashboard", "layout");
}

// ── Disconnecting a server (SIGMA-204, SIGMA-205) ───────────────────────────
//
// This used to be one action that deleted the row and showed a toast claiming
// "the agent tears down its WireGuard tunnel". It did not: the binary, the
// systemd unit, the tunnel, the containers and the volumes all stayed on the
// machine, and the only thing that changed was that we stopped being able to
// see them. It is now two actions, because there are genuinely two things an
// operator can mean.

/** The shape both disconnect actions answer with. `boundResources` is the 409's
 *  data — the resources still on the host — so the dialog lists them by name
 *  instead of printing a control-plane error string at the operator. */
export type DisconnectResult =
  | { ok: true; status: string }
  | { ok: false; error: string; boundResources: string[] };

/** Resolve the org for a server that may exist only in the control plane: a
 *  connected-but-unattached server has no local mirror row, so a disconnect
 *  must not be gated on one. */
async function serverOrgId(serverId: string) {
  const [server] = await db.select().from(s.servers).where(eq(s.servers.id, serverId));
  const orgId = server?.orgId ?? (await getActiveOrgId());
  return { server, orgId };
}

/** Resources still bound to a server — the first line of defence, in demo mode
 *  as in CP mode. The control plane runs its own version of this check inside
 *  the disconnect transaction (where it is race-free against a concurrent
 *  create); this is the demo mirror of it, so the demo walks the same refusal
 *  rather than silently orphaning resources. */
async function boundResourceNames(serverId: string): Promise<string[]> {
  const rows = await db
    .select({ name: s.resources.name })
    .from(s.resources)
    .where(eq(s.resources.serverId, serverId));
  return rows.map((r) => r.name).sort();
}

/** Graceful decommission: ask the agent to remove the workloads and then
 *  itself, and tombstone the record only once it confirms (or the control
 *  plane's timeout gives up).
 *
 *  purgeVolumes defaults OFF at every layer, this one included. Named volumes
 *  are database data directories and uploaded files — the customer's, not the
 *  machine's. */
export async function decommissionServer(input: {
  serverId: string;
  purgeVolumes?: boolean;
}): Promise<DisconnectResult> {
  const { server, orgId } = await serverOrgId(input.serverId);
  if (!orgId) return { ok: false, error: "No active organization.", boundResources: [] };
  // Destructive — Project Admin (P1-4), not bare membership.
  const { user, role } = await requireProjectAdmin(orgId);
  const purgeVolumes = input.purgeVolumes === true;

  if (cpEnabled()) {
    try {
      const res = await cpDecommissionServer(orgId, input.serverId, purgeVolumes, {
        name: user.name,
        role,
      });
      revalidatePath("/dashboard", "layout");
      return { ok: true, status: res.status };
    } catch (err) {
      return {
        ok: false,
        error: err instanceof Error ? err.message : "Please try again.",
        boundResources: boundResourcesOf(err),
      };
    }
  }

  // Demo mode: no control plane and no agent, so the row carries the in-flight
  // state and the dashboard offers the buttons that drive it forward.
  if (!server) return { ok: false, error: "Server not found.", boundResources: [] };
  const bound = await boundResourceNames(input.serverId);
  if (bound.length > 0) {
    return {
      ok: false,
      error: `Server has ${bound.length} bound resource(s): ${bound.join(", ")}`,
      boundResources: bound,
    };
  }
  await db
    .update(s.servers)
    .set({
      status: SERVER_STATUS.decommissioning,
      decommissionStartedAt: new Date(),
      decommissionPurgeVolumes: purgeVolumes,
    })
    .where(eq(s.servers.id, input.serverId));
  await writeAudit({
    orgId,
    actor: user.name,
    action: purgeVolumes
      ? "Server decommissioning (application data included)"
      : "Server decommissioning",
    target: server.name,
  });
  revalidatePath("/dashboard", "layout");
  return { ok: true, status: SERVER_STATUS.decommissioning };
}

/** Force disconnect: remove the record here and revoke the agent's credential,
 *  leaving the host untouched.
 *
 *  Offered only where a graceful teardown cannot land — an unreachable machine,
 *  or one whose decommission timed out — and always alongside the manual
 *  cleanup script, because everything SigmaHub installed is still on that box.
 *  It is deliberately NOT the default: it is one click and it "works", and
 *  every use of it recreates the defect this feature exists to fix. */
export async function forceDisconnectServer(input: { serverId: string }): Promise<DisconnectResult> {
  const { server, orgId } = await serverOrgId(input.serverId);
  if (!orgId) return { ok: false, error: "No active organization.", boundResources: [] };
  const { user, role } = await requireProjectAdmin(orgId);

  if (cpEnabled()) {
    try {
      await cpDeleteServer(orgId, input.serverId, { name: user.name, role });
    } catch (err) {
      return {
        ok: false,
        error: err instanceof Error ? err.message : "Please try again.",
        boundResources: boundResourcesOf(err),
      };
    }
  } else {
    if (!server) return { ok: false, error: "Server not found.", boundResources: [] };
    const bound = await boundResourceNames(input.serverId);
    if (bound.length > 0) {
      return {
        ok: false,
        error: `Server has ${bound.length} bound resource(s): ${bound.join(", ")}`,
        boundResources: bound,
      };
    }
  }
  if (server) {
    await db.delete(s.servers).where(eq(s.servers.id, input.serverId));
  }
  await writeAudit({
    orgId,
    actor: user.name,
    action: "Server force-disconnected — the host was not cleaned up",
    target: server?.name ?? input.serverId,
  });
  revalidatePath("/dashboard", "layout");
  return { ok: true, status: "deleted" };
}

/** Demo-mode only: drive a decommission forward without a real agent.
 *
 *  Demo mode is where a prospective user learns what these states MEAN, so it
 *  has to walk the whole flow and not just the happy half — including the two
 *  ways a graceful teardown does not land (SIGMA-215).
 *
 *   - "ack"     the agent finished and reported: the record is removed;
 *   - "failed"  the agent reported a teardown it could not complete: the record
 *               is still removed (the machine has already gone), and the audit
 *               says so — this is what the cleanup script exists for;
 *   - "timeout" the agent never answered: the decommission ages past the
 *               control plane's window, so the dialog starts offering Force
 *               disconnect;
 *   - "silence" the host stops heartbeating entirely, which is the OTHER route
 *               to the force path — nothing is listening to be asked. */
export async function simulateDecommission(input: {
  serverId: string;
  event: "ack" | "failed" | "timeout" | "silence";
}) {
  if (cpEnabled()) return;
  const [server] = await db.select().from(s.servers).where(eq(s.servers.id, input.serverId));
  if (!server) throw new Error("Server not found.");
  // Project Admin, matching decommissionServer and forceDisconnectServer. The
  // "ack"/"failed" branches DELETE the server row, and a demo where a Viewer
  // can remove a server teaches the wrong permission model to the person
  // evaluating the product — the one audience demo mode exists for.
  const { user } = await requireProjectAdmin(server.orgId);
  // Every event but "silence" is a report ABOUT a teardown, and two of them
  // delete the row. A report on a teardown that was never requested is not a
  // simulation of anything the product does, and the caller that sends one is
  // wrong about the server's state — so say so instead of removing a server on
  // the strength of it. The servers page used to fire "ack" at a row it had
  // simply arrived late to, and this is the second line of defence against that
  // class of caller: a demo fleet must not shrink by one because a page loaded.
  if (input.event !== "silence" && server.status !== SERVER_STATUS.decommissioning) {
    throw new Error(
      `${server.name} is not being decommissioned, so there is no teardown to report on. ` +
        "Open the server and press Disconnect first."
    );
  }
  // And a teardown past the control plane's window is no longer one an agent
  // can report on. This is the half the status check above cannot see, and the
  // half the comment above used to claim it covered: a page arriving late finds
  // a row that IS `decommissioning`, so the status alone waves it straight
  // through. Adversarial review drove exactly that — the seeded fixture's own
  // shape, five minutes old and still `decommissioning` — and the ack was
  // accepted and the server deleted.
  //
  // Past the window the honest outcome is not a tombstone written on the
  // agent's behalf; it is Force disconnect and the cleanup script, which is
  // what the dialog already offers by then. `timeout` is excluded because
  // ageing the row past the window is its entire job.
  if (
    (input.event === "ack" || input.event === "failed") &&
    forceReason({
      status: server.status,
      decommissioningSince: server.decommissionStartedAt,
    }) !== null
  ) {
    throw new Error(
      `Nothing answered for ${server.name} inside the control plane's window, so this teardown ` +
        "cannot be confirmed on the agent's behalf. Open the server and use Force disconnect, " +
        "which also gives you the manual cleanup script."
    );
  }

  switch (input.event) {
    case "ack":
    case "failed": {
      await db.delete(s.servers).where(eq(s.servers.id, input.serverId));
      await writeAudit({
        orgId: server.orgId,
        actor: user.name,
        action:
          input.event === "ack"
            ? "Server decommissioned"
            : "Server decommissioned with errors — containers: docker daemon not reachable",
        target: server.name,
      });
      break;
    }
    case "timeout": {
      // Age the request past the control plane's window. The dialog decides
      // what to offer by comparing this against DECOMMISSION_TIMEOUT_MS, so
      // moving the clock is the whole simulation.
      await db
        .update(s.servers)
        .set({ decommissionStartedAt: new Date(Date.now() - DECOMMISSION_TIMEOUT_MS - 60_000) })
        .where(eq(s.servers.id, input.serverId));
      break;
    }
    case "silence": {
      await db
        .update(s.servers)
        .set({ status: SERVER_STATUS.unreachable })
        .where(eq(s.servers.id, input.serverId));
      await writeAudit({
        orgId: server.orgId,
        actor: "sweeper",
        action: "Server unreachable",
        target: server.name,
      });
      break;
    }
  }
  revalidatePath("/dashboard", "layout");
}
