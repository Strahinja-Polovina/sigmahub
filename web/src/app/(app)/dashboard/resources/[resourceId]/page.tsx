import { notFound, redirect } from "next/navigation";
import { getActiveOrgId, requireMembership } from "@/server/active-org";
import { getResourceDetail } from "@/server/queries";
import { effectiveSecrets } from "@/server/secrets-data";
import { cpEnabled, cpListDomains, cpListDeployments, cpRollbackTargets, cpBackupPolicy } from "@/server/cp";
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
  const { role } = await requireMembership(orgId);
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

  // Backup policy for DB kinds (CP mode). A CP failure degrades to no panel row.
  const dbKinds = new Set(["postgres", "mysql", "mongo", "redis"]);
  let backupPolicy = null;
  if (cpEnabled() && dbKinds.has(detail.resource.kind)) {
    try {
      const p = await cpBackupPolicy(orgId, resourceId);
      backupPolicy = { schedule: p.schedule, retentionDays: p.retentionDays, enabled: p.enabled };
    } catch {
      backupPolicy = null;
    }
  }

  return (
    <ResourceDetail
      detail={{ ...detail, secrets, canManage }}
      orgId={orgId}
      domains={domains}
      domainsEnabled={cpEnabled()}
      cpDeployments={deployments}
      rollbackTargetIds={rollbackTargetIds}
      deploymentsEnabled={cpEnabled() && detail.resource.kind === "app"}
      backupPolicy={backupPolicy}
      databasesEnabled={cpEnabled()}
    />
  );
}
