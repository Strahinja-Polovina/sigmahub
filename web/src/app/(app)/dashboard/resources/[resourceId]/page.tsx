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
  cpGetComposeServices,
  cpListServers,
  cpListBackupTargets,
  cpListBackupRuns,
  cpListGitConnectionsWithMaps,
  cpGetLLM,
  type CpDatabaseInfo,
  type CpHealthCheck,
  type CpS3Info,
  type CpLLMInfo,
  type CpBackupTarget,
  type CpBackupRun,
} from "@/server/cp";
import { isDatabaseEngine } from "@/lib/server-catalog.generated";
import { getDatabaseInfo } from "@/server/actions/databases";
import { getS3Info } from "@/server/actions/s3";
import { loadResourceTelemetry } from "@/server/resource-telemetry";
import type { AutoDeployPolicy } from "@/components/dashboard/resources/resource-detail";
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

/** Load a database resource's connection metadata (P1-10). A CP failure
 *  degrades to null rather than breaking the page.
 *
 *  Both modes since SIGMA-215: the action derives a demo engine's details from
 *  the resource id, so the panel — the screen that says a managed database is
 *  mesh-only and hands out an audited credential — is reachable offline.
 *
 *  Which kinds have connection details is the control plane's engine table, so
 *  it is asked rather than restated. This module kept its own Set of the four
 *  engine kinds, a second copy of the one in resource-detail.tsx and in a file
 *  that never sees it; a fifth engine added to the Go catalog would have been
 *  provisioned by the control plane and then opened to a page with no Database
 *  panel at all, because the loader in front of it had never heard of it
 *  (SIGMA-216). */
async function loadDatabase(
  failures: LoadFailures,
  orgId: string,
  resourceId: string,
  kind: string
): Promise<CpDatabaseInfo | null> {
  if (!isDatabaseEngine(kind)) return null;
  return attempt(
    failures,
    "connection details",
    () => getDatabaseInfo({ orgId, resourceId }),
    null
  );
}

/** Load an S3 resource's endpoint metadata (P2-1), likewise in both modes. */
async function loadS3(
  failures: LoadFailures,
  orgId: string,
  resourceId: string,
  kind: string
): Promise<CpS3Info | null> {
  if (kind !== "s3") return null;
  return attempt(failures, "endpoint details", () => getS3Info({ orgId, resourceId }), null);
}

/** Load a model endpoint's readout (SIGMA-303).
 *
 *  Control plane only, and deliberately: the endpoint is a real port allocated
 *  on a real GPU host from MESH_PORT_BASE upward, so there is nothing offline to
 *  derive it from the way demoS3Info derives an S3 endpoint. A CP failure goes
 *  into loadFailures rather than degrading to null, because "no endpoint" and
 *  "we could not read the endpoint" are the two things a user acting on this
 *  page must not confuse — the first reads as a model that never came up. */
async function loadLLM(
  failures: LoadFailures,
  orgId: string,
  resourceId: string,
  kind: string
): Promise<CpLLMInfo | null> {
  if (!cpEnabled() || kind !== "llm") return null;
  return attempt(failures, "endpoint details", () => cpGetLLM(orgId, resourceId), null);
}

/** The live per-resource failure the agent reported (mesh bind, image pull,
 *  health-check timeout…). The web mirror only stores a coarse status string,
 *  so the actionable reason lives in the CP resource's status object — surface
 *  it so an errored resource explains itself instead of showing a blank logs
 *  panel. A CP failure degrades to null. */
async function loadCpResourceFacts(
  orgId: string,
  resourceId: string,
  environmentId: string
): Promise<{ statusError: string | null; healthCheck: CpHealthCheck | null }> {
  const none = { statusError: null, healthCheck: null };
  if (!cpEnabled()) return none;
  try {
    const resources = await cpListResources(orgId, environmentId);
    const res = resources.find((r) => r.id === resourceId);
    const st = res?.status;
    const err = st && typeof st === "object" ? (st as Record<string, unknown>).error : undefined;
    // The probe the reconciler actually runs, as the CP stored it on the spec
    // (createResourceBody copies gitdetect's healthCheck through). The Settings
    // tab used to claim a constant "Enabled" here (SIGMA-240).
    const hc = res?.spec?.healthCheck;
    return {
      statusError: typeof err === "string" && err.trim() ? err : null,
      healthCheck: hc && typeof hc === "object" ? (hc as CpHealthCheck) : null,
    };
  } catch {
    return none;
  }
}

/** The branch mapping that governs whether a push deploys into this resource's
 *  environment (SIGMA-240). CP mode + app resources only: a database has no
 *  repository, and demo mode has no git connections. A CP failure degrades to
 *  null, which the UI renders as "No branch mapped" — never as "Enabled". */
async function loadAutoDeploy(
  orgId: string,
  projectId: string,
  environmentId: string,
  kind: string,
  repo: string | null
): Promise<AutoDeployPolicy | null> {
  if (!cpEnabled() || kind !== "app" || !repo) return null;
  try {
    const conns = await cpListGitConnectionsWithMaps(orgId, projectId);
    for (const { branchMaps } of conns) {
      const map = branchMaps.find((m) => m.environmentId === environmentId);
      if (map) return { branch: map.branch, policy: map.policy };
    }
    return null;
  } catch {
    return null;
  }
}

/** Load a database's backup targets + run history (P1-11, CP mode only). */
async function loadBackups(
  orgId: string,
  resourceId: string,
  isDatabase: boolean,
  failures: LoadFailures
): Promise<{ targets: CpBackupTarget[]; runs: CpBackupRun[] }> {
  // Still CP-only, and now explicitly rather than by accident: the database
  // panel renders in demo mode, so this had to stop being "whatever cpGetDatabase
  // returned". Backups are executed by the control plane against a real S3
  // target — there is nothing offline to schedule, verify or restore — so the
  // panel states that instead of offering buttons that throw (SIGMA-215).
  if (!cpEnabled() || !isDatabase) return { targets: [], runs: [] };
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
  const llm = await loadLLM(loadFailures, orgId, resourceId, detail.resource.kind);
  const backups = await loadBackups(orgId, resourceId, database !== null, loadFailures);
  // Telemetry reports its own read failure into loadFailures like every other
  // loader here — an unreachable control plane must not render as "the pipeline
  // is configured and nothing arrived" (SIGMA-236).
  const telemetry = await loadResourceTelemetry(orgId, resourceId, loadFailures);
  const { statusError, healthCheck } = await loadCpResourceFacts(
    orgId,
    resourceId,
    detail.resource.environmentId
  );
  const autoDeploy = await loadAutoDeploy(
    orgId,
    detail.resource.projectId,
    detail.resource.environmentId,
    detail.resource.kind,
    detail.resource.repo
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
          (await cpListServers(orgId)).map((sv) => ({
            id: sv.id,
            name: sv.name,
            type: sv.type,
            // Carried so the restore dialogs can refuse a dead target
            // (SIGMA-241) instead of queueing an op nobody will poll for.
            status: sv.status,
            // The last heartbeat, which only the control plane has — the web
            // mirror keeps no such column. It is what lets the host banner say
            // "since 08:14" rather than a vague "is not answering" (SIGMA-251).
            lastSeenAt: sv.lastSeenAt,
            // The edge role, which decides whether a custom domain on this
            // host can ever get a certificate: the reconciler renders Traefik
            // (and its ACME client) onto proxy-role servers only, so the
            // domains panel has to be able to say so BEFORE the operator edits
            // their DNS and waits (SIGMA-316).
            proxyRole: sv.proxyRole ?? false,
          })),
        [] as {
          id: string;
          name: string;
          type: string;
          status: string;
          lastSeenAt: string | null;
          proxyRole: boolean;
        }[])
      : Promise.resolve([]),
  ]);

  // The host's live state, preferred over the reconciled mirror when the control
  // plane answered. Both carry a status; only the CP carries a heartbeat, and a
  // resource stuck on a silent machine is exactly the case where the mirror is
  // most likely to be the stale copy (SIGMA-251).
  const cpHost = detail.server
    ? placementServers.find((sv) => sv.id === detail.server!.id)
    : undefined;
  const server = detail.server
    ? {
        ...detail.server,
        status: cpHost?.status ?? detail.server.status,
        lastSeenAt: cpHost?.lastSeenAt ?? null,
        // Left undefined when the control plane did not answer: the domains
        // panel warns on an explicit false only, so an unknown role stays quiet
        // rather than accusing every host of missing the edge role (SIGMA-316).
        proxyRole: cpHost?.proxyRole,
      }
    : null;

  return (
    <ResourceDetail
      detail={{ ...detail, server, secrets, canManage }}
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
      llm={llm}
      compose={compose}
      placementServers={placementServers}
      backupTargets={backups.targets}
      backupRuns={backups.runs}
      environmentId={detail.resource.environmentId}
      cpTelemetry={telemetry}
      autoDeploy={autoDeploy}
      healthCheck={healthCheck}
    />
  );
}
