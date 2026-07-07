import "server-only";
import { desc, eq } from "drizzle-orm";
import { db } from "./db";
import * as s from "./db/schema";

function rid() {
  return `aud_${crypto.randomUUID().replace(/-/g, "").slice(0, 14)}`;
}

/** Append an audit row. Best-effort: an audit failure must never break the
 *  mutation that triggered it, so errors are swallowed. */
export async function writeAudit(input: {
  orgId: string;
  actor: string;
  action: string;
  target?: string;
}) {
  try {
    await db.insert(s.auditLog).values({
      id: rid(),
      orgId: input.orgId,
      actor: input.actor,
      action: input.action,
      target: input.target ?? "",
    });
  } catch {
    // non-fatal
  }
}

export async function getAuditLog(orgId: string, limit = 50) {
  return db
    .select()
    .from(s.auditLog)
    .where(eq(s.auditLog.orgId, orgId))
    .orderBy(desc(s.auditLog.createdAt))
    .limit(limit);
}
