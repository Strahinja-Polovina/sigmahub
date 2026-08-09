"use server";

import { eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireProjectAdminForResource, requireResourceVisible } from "../active-org";
import { writeAudit } from "../audit";
import { demoDatabaseConnection, demoDatabaseInfo } from "@/lib/demo-connection";
import {
  cpEnabled,
  cpGetDatabase,
  cpRevealDatabaseConnection,
  type CpDatabaseInfo,
  type CpDatabaseConnection,
} from "../cp";

/** The local row a demo connection is derived from: the resource itself, plus
 *  the mesh address of the host it landed on — a managed engine binds to the
 *  private mesh and nothing else, which is the fact the panel exists to state. */
async function demoResource(resourceId: string) {
  const [row] = await db
    .select({
      id: s.resources.id,
      name: s.resources.name,
      kind: s.resources.kind,
      meshIp: s.servers.meshIp,
    })
    .from(s.resources)
    .leftJoin(s.servers, eq(s.resources.serverId, s.servers.id))
    .where(eq(s.resources.id, resourceId));
  return row;
}

/** Non-secret connection metadata + backup policy. Visible to any member who can
 *  see the resource's project (P2-7, SIGMA-84).
 *
 *  Real provisioning lives in the control plane (P1-10). Demo mode has no
 *  engine, so it answers with values DERIVED from the resource id — stable, and
 *  with every secret visibly marked (see @/lib/demo-connection). It used to
 *  throw, which meant the panel never mounted and someone evaluating the
 *  product offline never learned that a managed database hands them a
 *  connection string at all (SIGMA-215). */
export async function getDatabaseInfo(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpDatabaseInfo | null> {
  await requireResourceVisible(input.orgId, input.resourceId);
  if (cpEnabled()) return cpGetDatabase(input.orgId, input.resourceId);
  const row = await demoResource(input.resourceId);
  if (!row) return null;
  return demoDatabaseInfo({
    resourceId: row.id,
    resourceName: row.name,
    kind: row.kind,
    meshIp: row.meshIp,
  });
}

/** Audited credential reveal. Project Admin+ — a Developer is rejected here
 *  AND by the control plane route (defense in depth), and every successful
 *  reveal writes an audit row on both sides.
 *
 *  The demo branch keeps BOTH halves of that: the same gate and the same audit
 *  row. A demo where any member can read a database password teaches the wrong
 *  permission model to the one audience demo mode exists for. */
export async function revealDatabaseConnection(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpDatabaseConnection> {
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  let conn: CpDatabaseConnection;
  if (cpEnabled()) {
    conn = await cpRevealDatabaseConnection(input.orgId, input.resourceId, {
      name: user.name,
      role,
    });
  } else {
    const row = await demoResource(input.resourceId);
    const demo = row
      ? demoDatabaseConnection({
          resourceId: row.id,
          resourceName: row.name,
          kind: row.kind,
          meshIp: row.meshIp,
        })
      : null;
    if (!demo) throw new Error("This resource is not a managed database.");
    conn = demo;
  }
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "DB credentials revealed",
    target: input.resourceId,
  });
  return conn;
}
