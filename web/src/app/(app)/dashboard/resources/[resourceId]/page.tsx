import { notFound, redirect } from "next/navigation";
import { getActiveOrgId, requireMembership, visibleProjects } from "@/server/active-org";
import { getResourceDetail } from "@/server/queries";
import { effectiveSecrets } from "@/server/secrets-data";
import {
  cpEnabled,
  cpListDomains,
  cpListDeployments,
  cpRollbackTargets,
  cpGetDatabase,
  cpGetS3,
  cpListBackupTargets,
  cpListBackupRuns,
  cpQueryResourceMetrics,
  cpQueryLogs,
  cpKind,
  type CpDatabaseInfo,
  type CpS3Info,
  type CpBackupTarget,
  type CpBackupRun,
} from "@/server/cp";
import type { CpTelemetry } from "@/components/dashboard/resources/resource-detail";
import { ResourceDetail } from "@/components/dashboard/resources/resource-detail";
import type { DomainRow } from "@/components/dashboard/resources/resource-domains-panel";
import type { DeploymentRow } from "@/components/dashboard/resources/deployments-panel";

/** Load an app resource's custom domains (CP mode only). A CP failure degrades
 *  to an empty list rather than breaking the page. */
async function loadDomains(orgId: string, resourceId: string, kind: string): Promise<DomainRow[]> {
  if (!cpEnabled() || kind !== "app") return [];
  try {
    return (await cpListDomains(orgId, resourceId)).map((d) => ({
      id: d.id,
      domain: d.domain,
      certStatus: d.certStatus,
      certExpiresAt: d.certExpiresAt,
      lastError: d.lastError,
    }));
  } catch {
    return [];
  }
}

/** Load the CP release history + rollback candidates (CP mode only). A CP failure
 *  degrades to empty lists rather than breaking the page. */
async function loadDeployments(
  orgId: string,
  resourceId: string,
  kind: string
): Promise<{ deployments: DeploymentRow[]; rollbackTargetIds: string[] }> {
  if (!cpEnabled() || kind !== "app") return { deployments: [], rollbackTargetIds: [] };
  try {
    const [deps, targets] = await Promise.all([
      cpListDeployments(orgId, resourceId, 25),
      cpRollbackTargets(orgId, resourceId),
    ]);
    return {
      deployments: deps.map((d) => ({
        id: d.id,
        trigger: d.trigger,
        gitRef: d.gitRef,
        gitSha: d.gitSha,
        status: d.status,
        detail: d.detail,
        rollbackOf: d.rollbackOf,
        imageDigest: d.imageDigest,
        buildSeconds: d.buildSeconds,
        durationSeconds: d.durationSeconds,
        createdBy: d.createdBy,
        createdAt: d.createdAt,
        startedAt: d.startedAt,
        serviceStatus: d.serviceStatus,
      })),
      rollbackTargetIds: targets.map((t) => t.id),
    };
  } catch {
    return { deployments: [], rollbackTargetIds: [] };
  }
}

const DB_KINDS = new Set(["postgres", "mysql", "redis", "mongo", "mongodb"]);

/** Load a database resource's connection metadata (P1-10, CP mode only). A CP
 *  failure degrades to null rather than breaking the page. */
async function loadDatabase(
  orgId: string,
  resourceId: string,
  kind: string
): Promise<CpDatabaseInfo | null> {
  if (!cpEnabled() || !DB_KINDS.has(cpKind(kind))) return null;
  try {
    return await cpGetDatabase(orgId, resourceId);
  } catch {
    return null;
  }
}

/** Load an S3 resource's endpoint metadata (P2-1, CP mode only). */
async function loadS3(
  orgId: string,
  resourceId: string,
  kind: string
): Promise<CpS3Info | null> {
  if (!cpEnabled() || kind !== "s3") return null;
  try {
    return await cpGetS3(orgId, resourceId);
  } catch {
    return null;
  }
}

/** Load real pipeline telemetry (P1-13, CP mode only). pipeline=false renders
 *  the explicit not-configured state — CP mode never shows synthetic data. */
async function loadTelemetry(orgId: string, resourceId: string): Promise<CpTelemetry | null> {
  if (!cpEnabled()) return null;
  try {
    const [metrics, logs] = await Promise.all([
      cpQueryResourceMetrics(orgId, resourceId),
      cpQueryLogs(orgId, { resourceId, limit: 200 }),
    ]);
    return {
      pipeline: metrics !== null || logs !== null,
      metrics: metrics ?? [],
      logs: logs ?? [],
    };
  } catch {
    return { pipeline: true, metrics: [], logs: [] };
  }
}

/** Load a database's backup targets + run history (P1-11, CP mode only). */
async function loadBackups(
  orgId: string,
  resourceId: string,
  isDatabase: boolean
): Promise<{ targets: CpBackupTarget[]; runs: CpBackupRun[] }> {
  if (!isDatabase) return { targets: [], runs: [] };
  try {
    const [targets, runs] = await Promise.all([
      cpListBackupTargets(orgId),
      cpListBackupRuns(orgId, resourceId),
    ]);
    return { targets, runs };
  } catch {
    return { targets: [], runs: [] };
  }
}

export default async function ResourceDetailPage({
  params,
}: {
  params: Promise<{ resourceId: string }>;
}) {
  const { resourceId } = await params;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const detail = await getResourceDetail(resourceId);
  if (!detail || detail.orgId !== orgId) notFound();

  // Managing secrets (create/reveal/delete) is Project Admin+, matching the CP
  // route gates; a Developer sees masked metadata only.
  const { user, role } = await requireMembership(orgId);
  // P2-7 read scoping: a project-scoped user must not open a resource in a
  // project they were never granted, even inside their own org — this page
  // exposes DB/S3 metadata, deploy history and container logs (SIGMA-75).
  const visible = await visibleProjects(user.id, orgId, role);
  if (visible && !visible.has(detail.resource.projectId)) notFound();
  const canManage = role === "Org Admin" || role === "Project Admin";
  const secrets = await effectiveSecrets(
    orgId,
    detail.resource.projectId,
    detail.resource.environmentId
  );
  const domains = await loadDomains(orgId, resourceId, detail.resource.kind);
  const { deployments, rollbackTargetIds } = await loadDeployments(
    orgId,
    resourceId,
    detail.resource.kind
  );
  const database = await loadDatabase(orgId, resourceId, detail.resource.kind);
  const s3 = await loadS3(orgId, resourceId, detail.resource.kind);
  const backups = await loadBackups(orgId, resourceId, database !== null);
  const telemetry = await loadTelemetry(orgId, resourceId);

  return (
    <ResourceDetail
      detail={{ ...detail, secrets, canManage }}
      orgId={orgId}
      domains={domains}
      domainsEnabled={cpEnabled()}
      cpDeployments={deployments}
      rollbackTargetIds={rollbackTargetIds}
      deploymentsEnabled={cpEnabled() && detail.resource.kind === "app"}
      database={database}
      s3={s3}
      backupTargets={backups.targets}
      backupRuns={backups.runs}
      environmentId={detail.resource.environmentId}
      cpTelemetry={telemetry}
    />
  );
}
