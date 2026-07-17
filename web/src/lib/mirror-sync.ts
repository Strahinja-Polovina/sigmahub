// Pure mapping helpers for the CP → local read-model reconciliation
// (SIGMA-56). The dashboard's local mirror rows share ids with their CP
// entities, so mapping is by-id; these functions decide what an upserted row
// looks like and which local ids are stale. Kept free of server-only imports
// so the drift rules are unit-testable.

import type { CpEnvironment, CpProject, CpResource } from "@/server/cp";

/** Same slug rule the create-project action uses, for CP-created projects
 *  that have no local row yet (the CP has no slug concept). */
export function slugifyName(x: string): string {
  return (
    x
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 40) || "project"
  );
}

/** The CP kind vocabulary says "mongodb"; the local schema says "mongo". */
export function localResourceKind(cpKind: string): string {
  return cpKind === "mongodb" ? "mongo" : cpKind;
}

/** CP resource status is a JSON doc whose `state` becomes authoritative once
 *  the reconciler populates it; until then keep the existing mirror value. */
export function resourceStatusText(
  status: Record<string, unknown> | null | undefined,
  existing?: string | null
): string {
  const state =
    status && typeof status.state === "string" && status.state
      ? status.state
      : null;
  return state ?? existing ?? "provisioning";
}

/** Local mirror ids the CP no longer owns — the rows to tombstone. */
export function staleIds(
  localIds: Iterable<string>,
  cpIds: Iterable<string>
): string[] {
  const keep = new Set(cpIds);
  const stale: string[] = [];
  for (const id of localIds) if (!keep.has(id)) stale.push(id);
  return stale;
}

export function projectMirrorRow(
  cp: CpProject,
  existing?: { slug: string } | null
) {
  return {
    id: cp.id,
    orgId: cp.orgId,
    name: cp.name,
    // Slugs are local-only routing sugar: keep the one links already use.
    slug: existing?.slug ?? slugifyName(cp.name),
    description: cp.description ?? "",
    createdAt: new Date(cp.createdAt),
  };
}

export function environmentMirrorRow(cp: CpEnvironment) {
  return {
    id: cp.id,
    projectId: cp.projectId,
    name: cp.name,
    createdAt: new Date(cp.createdAt),
  };
}

export function resourceMirrorRow(
  cp: CpResource,
  existing?: {
    status: string;
    repo: string | null;
    domain: string | null;
    version: string | null;
    lastDeployAt: Date;
  } | null
) {
  const spec = (cp.spec ?? {}) as { repo?: unknown; domain?: unknown };
  const specStr = (v: unknown) => (typeof v === "string" && v ? v : null);
  return {
    id: cp.id,
    projectId: cp.projectId,
    environmentId: cp.environmentId,
    serverId: cp.serverId || null,
    name: cp.name,
    kind: localResourceKind(cp.kind),
    status: resourceStatusText(cp.status, existing?.status),
    // repo/domain ride the CP spec for web-created resources; local values
    // win when the spec doesn't carry them (e.g. database resources).
    repo: specStr(spec.repo) ?? existing?.repo ?? null,
    domain: specStr(spec.domain) ?? existing?.domain ?? null,
    version: existing?.version ?? "v1",
    lastDeployAt: existing?.lastDeployAt ?? new Date(cp.createdAt),
  };
}
