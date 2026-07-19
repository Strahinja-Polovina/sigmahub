"use server";

// S3 storage (P2-1): the info/reveal split mirrors databases — metadata for
// every member, the audited credential reveal for Project Admin+.

import { requireProjectAdminForResource, requireResourceVisible } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpGetS3,
  cpRevealS3Connection,
  cpListBuckets,
  cpCreateBucket,
  cpDeleteBucket,
  cpSetBucketQuota,
  cpCreateBucketKey,
  type CpBucket,
  type CpS3Connection,
  type CpS3Info,
} from "../cp";
import { revalidatePath } from "next/cache";

function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("S3 storage requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

export async function getS3Info(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpS3Info | null> {
  ensureCp();
  await requireResourceVisible(input.orgId, input.resourceId);
  return cpGetS3(input.orgId, input.resourceId);
}

export async function revealS3Connection(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpS3Connection> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const conn = await cpRevealS3Connection(input.orgId, input.resourceId, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "S3 credentials revealed",
    target: input.resourceId,
  });
  return conn;
}

// ── Bucket / key / quota CRUD (P2-1b, SIGMA-65) ─────────────────────────────
// Buckets are managed over the mesh by the agent's s3.configure op; these
// actions queue the work and the resource page reflects state as it converges.

export async function listBuckets(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpBucket[]> {
  ensureCp();
  await requireResourceVisible(input.orgId, input.resourceId);
  return cpListBuckets(input.orgId, input.resourceId);
}

export async function createBucket(input: {
  orgId: string;
  resourceId: string;
  name: string;
}): Promise<CpBucket> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const bucket = await cpCreateBucket(input.orgId, input.resourceId, input.name.trim(), { name: user.name, role });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "S3 bucket created", target: input.name.trim() });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return bucket;
}

export async function deleteBucket(input: {
  orgId: string;
  resourceId: string;
  bucket: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  await cpDeleteBucket(input.orgId, input.resourceId, input.bucket, { name: user.name, role });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "S3 bucket deleted", target: input.bucket });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
}

export async function setBucketQuota(input: {
  orgId: string;
  resourceId: string;
  bucket: string;
  quotaBytes: number;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  await cpSetBucketQuota(input.orgId, input.resourceId, input.bucket, input.quotaBytes, { name: user.name, role });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "S3 bucket quota set", target: `${input.bucket} · ${input.quotaBytes}` });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
}

/** Returns ONLY the new access key id — the secret is provisioned on the engine
 *  over the audited agent path and never returned to the browser. */
export async function createBucketKey(input: {
  orgId: string;
  resourceId: string;
  bucket: string;
}): Promise<{ accessKey: string }> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const out = await cpCreateBucketKey(input.orgId, input.resourceId, input.bucket, { name: user.name, role });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "S3 bucket key created", target: `${input.bucket} · ${out.accessKey}` });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return out;
}
