import { describe, it, expect, vi } from "vitest";
import * as React from "react";
import { renderToStaticMarkup } from "react-dom/server";

// The panel is a client component: it reaches for the router, the toaster and
// the backup server actions at import time. None of them can run here, and none
// of them is what these tests are about, so they are stubbed out.
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: () => {} }) }));
vi.mock("sonner", () => ({
  toast: Object.assign(() => {}, { success: () => {}, error: () => {} }),
}));
vi.mock("@/server/actions/backups", () => ({
  createBackupTarget: vi.fn(),
  listBackupRuns: vi.fn(),
  restoreDatabase: vi.fn(),
  restoreDatabaseToTimestamp: vi.fn(),
  setBackupPolicy: vi.fn(),
}));

import { DatabaseBackupsPanel, planRestoreTarget } from "./database-backups-panel";
import type { CpBackupRun } from "@/server/cp";

/** The scenario the restore path exists for: the database's host died and the
 *  control plane swept it to `unreachable`; the operator has already connected
 *  a replacement. */
const DEAD = { id: "srv-dead", name: "db-01", type: "database", status: "unreachable" };
const REPLACEMENT = { id: "srv-new", name: "db-02", type: "database", status: "running" };

const GOOD_BACKUP = {
  id: "run-1",
  kind: "backup",
  status: "success",
  snapshotId: "abcdef1234",
  createdAt: "2026-08-09T02:00:00Z",
  detail: "",
} as unknown as CpBackupRun;

function renderPanel(overrides: Record<string, unknown> = {}) {
  return renderToStaticMarkup(
    React.createElement(DatabaseBackupsPanel, {
      orgId: "org-1",
      resourceId: "res-1",
      environmentId: "env-1",
      serverId: DEAD.id,
      resourceName: "orders",
      policy: null,
      targets: [],
      runs: [GOOD_BACKUP],
      canManage: true,
      servers: [DEAD, REPLACEMENT],
      ...overrides,
    } as never)
  );
}

describe("DatabaseBackupsPanel restore targeting", () => {
  it("a restore can target a server other than the source's", () => {
    // 1. The restore has to survive the source losing its server. That is the
    //    only state a restore-into-new-database is ever reached from in anger,
    //    and the panel used to hide the whole control when serverId was null.
    expect(renderPanel({ serverId: null })).toContain("Restore to new");

    // 2. And it has to be aimable: a target chosen in the dialog is what gets
    //    submitted, not the source resource's own (dead) server.
    const plan = planRestoreTarget(REPLACEMENT.id, [DEAD, REPLACEMENT]);
    expect(plan).toEqual({ ok: true, serverId: REPLACEMENT.id });
    expect(plan.ok && plan.serverId).not.toBe(DEAD.id);

    // A server id the org list doesn't know (the list failed to load) is
    // deferred to the control plane rather than refused here.
    const unknown = planRestoreTarget("srv-mystery", []);
    expect(unknown).toEqual({ ok: true, serverId: "srv-mystery" });
  });

  it("an unreachable target is refused with a reason", () => {
    const plan = planRestoreTarget(DEAD.id, [DEAD, REPLACEMENT]);
    expect(plan.ok).toBe(false);
    // The refusal has to name the host and say why, otherwise it reads as
    // "your backups are broken" on the one day they are not.
    const reason = plan.ok ? "" : plan.reason;
    expect(reason).toContain(DEAD.name);
    expect(reason).toContain("unreachable");

    // A host tearing itself down is the same story: nothing will poll for the op.
    const decom = planRestoreTarget("srv-bye", [
      { id: "srv-bye", name: "db-03", type: "database", status: "decommissioning" },
    ]);
    expect(decom.ok).toBe(false);

    // Choosing nothing is refused too — the CP rejects an empty serverId with a
    // 400 that says nothing useful.
    expect(planRestoreTarget("", [DEAD, REPLACEMENT]).ok).toBe(false);

    // A healthy host is not refused.
    expect(planRestoreTarget(REPLACEMENT.id, [DEAD, REPLACEMENT]).ok).toBe(true);
  });
});
