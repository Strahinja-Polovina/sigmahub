import { notFound, redirect } from "next/navigation";
import { getActiveOrgId, requireMembership, projectGrants } from "@/server/active-org";
import { effectiveProjectRole, roleAtLeast } from "@/lib/rbac";
import { getResourceDetail } from "@/server/queries";
import { effectiveSecrets } from "@/server/secrets-data";
import {
  cpEnabled,
  cpListDomains,
  cpListDeployments,
  cpListResources,
  cpRollbackTargets,
  cpGetDatabase,
  cpGetS3,
  cpGetComposeServices,
  cpListServers,
  cpListBackupTargets,
  cpListBackupRuns,
  cpQueryResourceMetrics,
  cpQueryLogs,
  type CpDatabaseInfo,
  type CpS3Info,
  type CpBackupTarget,
  type CpBackupRun,
} from "@/server/cp";
import type { CpTelemetry } from "@/components/dashboard/resources/resource-detail";
import { ResourceDetail } from "@/components/dashboard/resources/resource-detail";
import type { DomainRow } from "@/components/dashboard/resources/resource-domains-panel";
import type { DeploymentRow } from "@/components/dashboard/resources/deployments-panel";


/**
 * Reads that failed, so the page can say so.
 *
 * Every loader here degraded a control-plane failure into an empty list or a
 * null — which renders as "no domains", "no releases", "no database". That is a
 * different statement from "we could not read them", and it is the one the user
 * acts on: they conclude the resource is fine and empty. Collecting the
 * failures lets the page distinguish the two instead of lying by omission.
 */
type LoadFailures = string[];

async function attempt<T>(
  failures: LoadFailures,
  what: string,
  fn: () => Promise<T>,
  fallback: T
): Promise<T> {
  try {
    return await fn();
  } catch {
    failures.push(what);
    return fallback;
  }
}

/** Load an app resource's custom domains (CP mode only). */
async function loadDomains(
  failures: LoadFailures,
  orgId: string,
  resourceId: string,
  kind: string
): Promise<DomainRow[]> {
  if (!cpEnabled() || kind !== "app") return [];
  return attempt(failures, "custom domains", async () =>
    (await cpListDomains(orgId, resourceId)).map((d) => ({
      id: d.id,
      domain: d.domain,
      certStatus: d.certStatus,
      certExpiresAt: d.certExpiresAt,
      lastError: d.lastError,
    })), []);
}

/** Load the CP release history + rollback candidates (CP mode only). A CP failure
 *  degrades to empty lists rather than breaking the page. */
async function loadDeployments(
  failures: LoadFailures,
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
    failures.push("release history");
    return { deployments: [], rollbackTargetIds: [] };
  }
}

const DB_KINDS = new Set(["postgres", "mysql", "redis", "mongodb"]);

/** Load a database resource's connection metadata (P1-10, CP mode only). A CP
 *  failure degrades to null rather than breaking the page. */
async function loadDatabase(
  failures: LoadFailures,
  orgId: string,
  resourceId: string,
  kind: string
): Promise<CpDatabaseInfo | null> {
  if (!cpEnabled() || !DB_KINDS.has(kind)) return null;
  return attempt(failures, "connection details", () => cpGetDatabase(orgId, resourceId), null);
}

/** Load an S3 resource's endpoint metadata (P2-1, CP mode only). */
async function loadS3(
  failures: LoadFailures,
  orgId: string,
  resourceId: string,
  kind: string
): Promise<CpS3Info | null> {
  if (!cpEnabled() || kind !== "s3") return null;
  return attempt(failures, "endpoint details", () => cpGetS3(orgId, resourceId), null);
}

/** The live per-resource failure the agent reported (mesh bind, image pull,
 *  health-check timeout…). The web mirror only stores a coarse status string,
 *  so the actionable reason lives in the CP resource's status object — surface
 *  it so an errored resource explains itself instead of showing a blank logs
 *  panel. A CP failure degrades to null. */
async function loadStatusError(
  orgId: string,
  resourceId: string,
  environmentId: string
): Promise<string | null> {
  if (!cpEnabled()) return null;
  try {
    const resources = await cpListResources(orgId, environmentId);
    const st = resources.find((r) => r.id === resourceId)?.status;
    const err = st && typeof st === "object" ? (st as Record<string, unknown>).error : undefined;
    return typeof err === "string" && err.trim() ? err : null;
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
  isDatabase: boolean,
  failures: LoadFailures
): Promise<{ targets: CpBackupTarget[]; runs: CpBackupRun[] }> {
  if (!isDatabase) return { targets: [], runs: [] };
  return attempt(failures, "backups", async () => {
    const [targets, runs] = await Promise.all([
      cpListBackupTargets(orgId),
      cpListBackupRuns(orgId, resourceId),
    ]);
    return { targets, runs };
  }, { targets: [], runs: [] });
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

  // P2-7: the EFFECTIVE role on this resource's project drives both gates. Read
  // visibility — a project-scoped user must not open a resource in a project
  // they were never granted, even inside their own org, since this page exposes
  // DB/S3 metadata, deploy history and container logs (SIGMA-75). And management
  // affordances — canManage uses the effective project role, not the bare org
  // role, so a user narrowed to Developer here sees masked metadata only
  // (SIGMA-82). Secret create/reveal/delete stay Project Admin+; the CP re-checks.
  const { user, role, scoped } = await requireMembership(orgId);
  const grants = await projectGrants(user.id, orgId);
  const effectiveRole = effectiveProjectRole(
    role,
    grants.get(detail.resource.projectId),
    scoped || grants.size > 0
  );
  if (!effectiveRole) notFound();
  const canManage = roleAtLeast(effectiveRole, "Project Admin");
  const secrets = await effectiveSecrets(
    orgId,
    detail.resource.projectId,
    detail.resource.environmentId
  );
  // Collected across every control-plane read so the page can distinguish
  // "empty" from "could not be read" instead of rendering both the same way.
  const loadFailures: LoadFailures = [];
  const domains = await loadDomains(loadFailures, orgId, resourceId, detail.resource.kind);
  const { deployments, rollbackTargetIds } = await loadDeployments(
    loadFailures,
    orgId,
    resourceId,
    detail.resource.kind
  );
  const database = await loadDatabase(loadFailures, orgId, resourceId, detail.resource.kind);
  const s3 = await loadS3(loadFailures, orgId, resourceId, detail.resource.kind);
  const backups = await loadBackups(orgId, resourceId, database !== null, loadFailures);
  const telemetry = await loadTelemetry(orgId, resourceId);
  const statusError = await loadStatusError(
    orgId,
    resourceId,
    detail.resource.environmentId
  );

  // Compose apps can spread their services across servers, so load the graph
  // and the org's servers to offer as placement targets. Not a Compose app (or
  // demo mode) → both stay empty and the panel doesn't render.
  const [compose, placementServers] = await Promise.all([
    cpEnabled() && detail.resource.kind === "app"
      ? attempt(loadFailures, "the service graph", () => cpGetComposeServices(orgId, detail.resource.id), null)
      : Promise.resolve(null),
    cpEnabled()
      ? attempt(loadFailures, "the server list", async () =>
          (await cpListServers(orgId)).map((sv) => ({ id: sv.id, name: sv.name, type: sv.type })),
        [] as { id: string; name: string; type: string }[])
      : Promise.resolve([]),
  ]);

  return (
    <ResourceDetail
      detail={{ ...detail, secrets, canManage }}
      statusError={statusError}
      loadFailures={loadFailures}
      orgId={orgId}
      domains={domains}
      domainsEnabled={cpEnabled()}
      cpDeployments={deployments}
      rollbackTargetIds={rollbackTargetIds}
      deploymentsEnabled={cpEnabled() && detail.resource.kind === "app"}
      database={database}
      s3={s3}
      compose={compose}
      placementServers={placementServers}
      backupTargets={backups.targets}
      backupRuns={backups.runs}
      environmentId={detail.resource.environmentId}
      cpTelemetry={telemetry}
    />
  );
}
