"use server";

// S3 storage (P2-1): the info/reveal split mirrors databases — metadata for
// every member, the audited credential reveal for Project Admin+.

import { and, eq } from "drizzle-orm";
import { db } from "../db";
import * as schema from "../db/schema";
import { requireProjectAdminForResource, requireResourceVisible } from "../active-org";
import { writeAudit } from "../audit";
import { demoS3Connection, demoS3Info } from "@/lib/demo-connection";
import {
  cpEnabled,
  cpGetS3,
  cpRevealS3Connection,
  cpListBuckets,
  cpCreateBucket,
  cpDeleteBucket,
  cpSetBucketQuota,
  cpCreateBucketKey,
  cpRevealBucketKey,
  type CpBucket,
  type CpS3Connection,
  type CpS3Info,
} from "../cp";
import { revalidatePath } from "next/cache";

/** Bucket, key and quota CRUD is the agent's s3.configure op talking to a real
 *  MinIO. There is no engine behind a demo resource for it to configure, and a
 *  bucket list that only ever agreed with itself would teach nothing — so this
 *  half stays control-plane-only, and the panel says so rather than offering
 *  buttons that throw (see S3Panel's `simulated` branch). */
function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Managing buckets requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** The local row a demo endpoint is derived from. */
// Org-scoped on purpose (SIGMA-365), for the reason spelled out on the twin in
// actions/databases.ts: requireProjectAdminForResource can fall back to the org
// gate for an id the mirror does not hold, and the demo branch has no control
// plane behind it to refuse a foreign resource.
async function demoResource(orgId: string, resourceId: string) {
  const [row] = await db
    .select({
      id: schema.resources.id,
      name: schema.resources.name,
      kind: schema.resources.kind,
      engine: schema.resources.engine,
      meshIp: schema.servers.meshIp,
    })
    .from(schema.resources)
    .innerJoin(schema.projects, eq(schema.resources.projectId, schema.projects.id))
    .leftJoin(schema.servers, eq(schema.resources.serverId, schema.servers.id))
    .where(and(eq(schema.resources.id, resourceId), eq(schema.projects.orgId, orgId)));
  return row;
}

/** Endpoint metadata, member-visible. Demo mode derives it from the resource id
 *  rather than throwing, for the same reason the database panel does: the
 *  wizard's last screen and the resource's Storage panel are where an operator
 *  learns that provisioning object storage hands them an endpoint and a key
 *  pair, and neither could render at all offline (SIGMA-215). */
export async function getS3Info(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpS3Info | null> {
  await requireResourceVisible(input.orgId, input.resourceId);
  if (cpEnabled()) return cpGetS3(input.orgId, input.resourceId);
  const row = await demoResource(input.orgId, input.resourceId);
  if (!row || row.kind !== "s3") return null;
  // The engine the resource was CREATED with, not the catalog default: this
  // panel is where a demo user finds out what their SeaweedFS pick got them,
  // and it used to answer MinIO for every one of them (SIGMA-215).
  return demoS3Info({
    resourceId: row.id,
    resourceName: row.name,
    engine: row.engine ?? undefined,
    meshIp: row.meshIp,
  });
}

export async function revealS3Connection(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpS3Connection> {
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  let conn: CpS3Connection;
  if (cpEnabled()) {
    conn = await cpRevealS3Connection(input.orgId, input.resourceId, {
      name: user.name,
      role,
    });
  } else {
    const row = await demoResource(input.orgId, input.resourceId);
    if (!row || row.kind !== "s3") throw new Error("This resource is not object storage.");
    conn = demoS3Connection({
      resourceId: row.id,
      resourceName: row.name,
      engine: row.engine ?? undefined,
      meshIp: row.meshIp,
    });
  }
  // Audited in both modes: the reveal is a privileged read, and a demo that
  // did not write the row would leave the audit log looking like nobody had
  // ever looked at a secret.
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

/** Returns ONLY the new access key id: at this point the op that programs the
 *  key into the engine has not run yet, so there is no active credential to
 *  hand over. The secret becomes readable through revealBucketKey once the key
 *  is recorded (SIGMA-313). */
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

/** Opens a bucket's scoped secret for a Project Admin+, audited on both sides.
 *
 *  SIGMA-313: minting a per-bucket key used to be a one-way door. The CP sealed
 *  the secret under the org DEK and released it only to the executing agent, the
 *  create response carried the access key alone, and the panel's Key button
 *  disappeared as soon as the key was recorded — no reveal, no re-mint, no
 *  route. The operator was left holding an access key with no secret and the
 *  only way to ship was the root credential this feature exists to avoid. */
export async function revealBucketKey(input: {
  orgId: string;
  resourceId: string;
  bucket: string;
}): Promise<{ bucket: string; accessKey: string; secretKey: string }> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const key = await cpRevealBucketKey(input.orgId, input.resourceId, input.bucket, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "S3 bucket key revealed",
    target: `${input.bucket} · ${key.accessKey}`,
  });
  return key;
}
