"use server";

import { revalidatePath } from "next/cache";
import { requireOrgAdmin } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpListServiceTokens,
  cpRotateServiceToken,
  cpRevokeServiceToken,
  type CpServiceToken,
} from "../cp";

// Service tokens are a control-plane concept (the org's web→CP credential), so
// these actions are CP-mode only; demo mode has no service tokens. All are Org
// Admin-gated (P1-4).

export async function listServiceTokens(orgId: string): Promise<CpServiceToken[]> {
  await requireOrgAdmin(orgId);
  if (!cpEnabled()) return [];
  return cpListServiceTokens(orgId);
}

export async function rotateServiceToken(input: { orgId: string; tokenId: string }) {
  const user = await requireOrgAdmin(input.orgId);
  if (!cpEnabled()) throw new Error("Service tokens require the control plane.");
  const rotated = await cpRotateServiceToken(input.orgId, input.tokenId, { name: user.name, role: "Org Admin" });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Rotated service token", target: rotated.name });
  revalidatePath("/dashboard/settings");
  return rotated; // { token, id, name, role } — the plaintext is shown once
}

export async function revokeServiceToken(input: { orgId: string; tokenId: string; name: string }) {
  const user = await requireOrgAdmin(input.orgId);
  if (!cpEnabled()) throw new Error("Service tokens require the control plane.");
  await cpRevokeServiceToken(input.orgId, input.tokenId, { name: user.name, role: "Org Admin" });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Revoked service token", target: input.name });
  revalidatePath("/dashboard/settings");
}
