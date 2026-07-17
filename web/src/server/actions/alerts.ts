"use server";

// Alerting (P2-6): channel CRUD + rules + test-fire. Channels receive
// org-wide operational events, so mutations are Org Admin; the CP enforces
// the same gate server-side via the signed actor role.

import { revalidatePath } from "next/cache";
import { requireMembership, requireOrgAdmin } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpCreateAlertChannel,
  cpDeleteAlertChannel,
  cpListAlertChannels,
  cpSetAlertRules,
  cpTestAlertChannel,
  type CpAlertChannel,
} from "../cp";

function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Alerting requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

export async function listAlertChannels(
  orgId: string
): Promise<{ channels: CpAlertChannel[]; events: string[] }> {
  ensureCp();
  await requireMembership(orgId);
  return cpListAlertChannels(orgId);
}

export async function createAlertChannel(input: {
  orgId: string;
  kind: string;
  name: string;
  config?: Record<string, unknown>;
  secret?: string;
}): Promise<CpAlertChannel> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  const ch = await cpCreateAlertChannel(
    input.orgId,
    { kind: input.kind, name: input.name, config: input.config, secret: input.secret },
    { name: user.name, role: "Org Admin" }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Alert channel created",
    target: `${input.name} (${input.kind})`,
  });
  revalidatePath("/dashboard/settings");
  return ch;
}

export async function deleteAlertChannel(input: {
  orgId: string;
  channelId: string;
  name: string;
}): Promise<void> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  await cpDeleteAlertChannel(input.orgId, input.channelId, { name: user.name, role: "Org Admin" });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Alert channel deleted",
    target: input.name,
  });
  revalidatePath("/dashboard/settings");
}

export async function setAlertRules(input: {
  orgId: string;
  channelId: string;
  events: string[];
}): Promise<void> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  await cpSetAlertRules(input.orgId, input.channelId, input.events, {
    name: user.name,
    role: "Org Admin",
  });
  revalidatePath("/dashboard/settings");
}

/** Fires a real test notification; throws the transport error on failure so
 *  the UI can show exactly why delivery does not work. */
export async function testAlertChannel(input: {
  orgId: string;
  channelId: string;
}): Promise<void> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  await cpTestAlertChannel(input.orgId, input.channelId, { name: user.name, role: "Org Admin" });
}
