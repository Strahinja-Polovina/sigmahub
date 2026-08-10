"use server";

import { revalidatePath } from "next/cache";
import { and, eq, ne } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireProjectAdminForResource, requireProjectRole } from "../active-org";
import { getProject, getResource } from "../queries";
import { writeAudit } from "../audit";
import { isIncompatible } from "@/lib/server-compat";
import { clusterCanHost, resourceKindLabel } from "@/lib/server-catalog.generated";
import { attachDomain } from "./domains";
import {
  buildResourceSpec,
  resolveDeployTarget,
  type DetectedComposeService,
} from "@/lib/deploy-spec";
import {
  cpEnabled,
  cpCreateResource,
  cpDeleteResource,
  cpMirrorServer,
  cpSelectGitRepo,
  cpRedeploy,
  cpRequestConfirmToken,
  cpConfirmDestructive,
} from "../cp";

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}
function sha7() {
  return crypto.randomUUID().replace(/-/g, "").slice(0, 7);
}

/** Deploy-from-Git result: create a resource and its first (running) deployment. */
export async function createResource(input: {
  projectId: string;
  environmentId: string;
  serverId?: string | null;
  /** Deploy INTO a Kubernetes cluster instead of onto one server — the
   *  scheduler picks the node. Mutually exclusive with serverId; the control
   *  plane refuses both and neither (SIGMA-200). */
  clusterId?: string | null;
  name: string;
  kind: string;
  repo?: string;
  domain?: string;
  /** Installation the repo was picked from (org-level GitHub integration).
   *  Present when the wizard used the repo picker rather than a pasted token. */
  installationId?: string;
  /** Repo config from the CP's git inspector, carried through the wizard.
   *  Without it the rendered rollout declares no ports and its health probe
   *  falls back to a port the app does not listen on, so the first deploy is
   *  guaranteed to fail the gate (SIGMA-160). */
  detected?: {
    ports?: number[];
    healthCheck?: { type: string; path?: string; port?: number; intervalSec?: number };
    /** The repo's Compose service graph. Dropping it made a repo that
     *  describes five services deploy as a single container: `spec.compose`
     *  was never written, so the reconciler's per-service branch had nothing
     *  to render from and could not be reached at all (SIGMA-199). */
    services?: DetectedComposeService[];
  };
  /** Port mappings as the wizard's networking step left them — host 0 is
   *  internal-only. Overrides `detected.ports` (SIGMA-210). */
  ports?: { container: number; host: number; protocol?: string }[];
  /** The build decision: method, Dockerfile path and build context. Without it
   *  a repo whose Dockerfile is not at the root cannot be deployed, even though
   *  the agent's build op has taken both fields all along (SIGMA-209). */
  build?: { method: string; dockerfile?: string; contextSubdir?: string };
  /** Inference runtime + model, for kind "llm". The control plane refuses an
   *  unknown runtime, so this is passed through rather than defaulted. */
  llm?: { engine: string; model: string };
  /** Object-storage engine, for kind "s3". Also refused by the CP when unknown,
   *  and defaulted there when absent. */
  s3Engine?: string;
}) {
  const project = await getProject(input.projectId);
  if (!project) throw new Error("Project not found.");
  const { user, role } = await requireProjectRole(project.orgId, input.projectId, "Project Admin");
  const name = input.name.trim();
  if (!name) throw new Error("Resource name is required.");
  // Exactly one deploy target, decided once and used everywhere below — the
  // ownership check, the CP call and the local mirror row all have to agree on
  // it. The control plane re-checks; this only says so a round trip earlier.
  const target = resolveDeployTarget({ serverId: input.serverId, clusterId: input.clusterId });
  if (!target.ok) throw new Error(target.error);

  // Don't trust the client-supplied env/server ids: they must belong to this
  // project/org, or a member of one org could plant a resource on another's
  // environment/server (IDOR).
  const [env] = await db
    .select({ projectId: s.environments.projectId })
    .from(s.environments)
    .where(eq(s.environments.id, input.environmentId));
  if (!env || env.projectId !== input.projectId) {
    throw new Error("Environment does not belong to this project.");
  }
  // Demo mode resolves the server from the local table; in CP mode servers
  // live in the control plane, so the ownership check + FK-satisfying local
  // mirror happen below (cpMirrorServer 404s cross-org ids).
  if (target.serverId && !cpEnabled()) {
    const [sv] = await db
      .select({ orgId: s.servers.orgId, status: s.servers.status, type: s.servers.type })
      .from(s.servers)
      .where(eq(s.servers.id, target.serverId));
    if (!sv || sv.orgId !== project.orgId) {
      throw new Error("Server does not belong to this organization.");
    }
    // The same refusal the control plane makes (store.CreateResource): a host
    // the enrollment gate rejected matches the availability matrix on paper and
    // not in fact, and scheduling onto it anyway reproduces exactly the failure
    // SIGMA-203 exists to move earlier (SIGMA-203).
    if (isIncompatible(sv.status)) {
      throw new Error(
        `That server is marked incompatible with its ${sv.type} type — change its type or disconnect it before scheduling work onto it.`
      );
    }
  }
  // The cluster half of the same check. In CP mode the control plane owns the
  // cluster and refuses a foreign id under the org token; demo clusters are
  // local rows with no such boundary unless it is drawn here (SIGMA-215).
  if (target.clusterId && !cpEnabled()) {
    const [cluster] = await db
      .select({ id: s.clusters.id, name: s.clusters.name, environmentId: s.clusters.environmentId })
      .from(s.clusters)
      .where(and(eq(s.clusters.id, target.clusterId), eq(s.clusters.orgId, project.orgId)));
    if (!cluster) throw new Error("Cluster does not belong to this organization.");
    // A cluster belongs to exactly one environment, and deploying into one from
    // a different environment would put the workload somewhere the environment
    // panel never shows it.
    if (cluster.environmentId !== input.environmentId) {
      throw new Error(`${cluster.name} belongs to a different environment.`);
    }
    // store.ClusterKindAllowed, from the catalog the control plane generates
    // its own copy of. The wizard already refuses these kinds on the target
    // step; this is the refusal that has to hold when the request did not come
    // from the wizard.
    if (!clusterCanHost(input.kind)) {
      throw new Error(
        `${resourceKindLabel(input.kind)} runs on its own server, not inside a cluster.`
      );
    }
  }

  // The spec is built by a pure helper so it is testable — see
  // buildResourceSpec, and the regression that motivated extracting it.
  const spec = buildResourceSpec(input);

  let id = rid("res");
  if (cpEnabled()) {
    // CP mode: the control plane owns the resource record and enforces the
    // kind/server-type availability matrix + env attachment server-side. The
    // local row mirrors it under the same id for read models; mirror the CP
    // server first so the local resources.server_id FK holds. A cluster
    // workload has no server to mirror — the scheduler picks its node — so the
    // local row simply carries no server_id.
    if (target.serverId) {
      await cpMirrorServer(project.orgId, target.serverId);
    }
    const created = await cpCreateResource(
      project.orgId,
      {
        environmentId: input.environmentId,
        serverId: target.serverId,
        clusterId: target.clusterId,
        name,
        kind: input.kind,
        spec,
      },
      { name: user.name, role }
    );
    id = created.id;

    // A repo-backed resource needs a git connection for push-to-deploy: branch
    // maps, webhook routing and clone credentials all hang off it. With the
    // org-level GitHub integration the user only PICKED a repo, so derive the
    // connection here instead of making them build one by hand in the Git panel.
    // Idempotent per (project, repo), and never fatal — the resource exists
    // either way and the Git panel can still connect it manually.
    if (input.repo) {
      try {
        await cpSelectGitRepo(
          project.orgId,
          {
            projectId: input.projectId,
            repoFullName: input.repo,
            installationId: input.installationId,
          },
          { name: user.name, role }
        );
      } catch (err) {
        console.warn("could not link repository to project", err);
      }
    }
  }
  const now = new Date();
  // CP mode reports honest state: the resource is provisioning until the
  // agent's status ingest flips it (the read path prefers live CP status).
  // Demo mode keeps the instant-running simulation.
  const cp = cpEnabled();
  await db.insert(s.resources).values({
    id,
    projectId: input.projectId,
    environmentId: input.environmentId,
    serverId: target.serverId ?? null,
    // Only in demo mode. The column references the LOCAL clusters table, which
    // exists to hold demo clusters; a CP-mode cluster id names a row in the
    // control plane's database and writing it here would break the FK. In CP
    // mode the control plane holds the target and the mirror is a read model,
    // which is already true of the compose placements and the domains.
    //
    // Demo mode had nowhere to put it at all, so a resource the user aimed at a
    // cluster was stored with neither a server nor a cluster and rendered as
    // unassigned — the wizard's choice reached the create call and evaporated.
    clusterId: cp ? null : target.clusterId ?? null,
    name,
    kind: input.kind,
    status: cp ? "provisioning" : "running",
    // Read off the SPEC rather than off the input, so the mirror cannot
    // disagree with what the control plane was sent about the same resource:
    // an object-storage engine and an inference runtime ride the one
    // `spec.engine` field, and buildResourceSpec is where that is decided.
    //
    // Demo mode wrote nothing here, and the S3 panel derives its image, port
    // and endpoint from the engine — so a user who picked SeaweedFS in the
    // wizard opened the resource and was told MinIO, the default (SIGMA-215).
    engine: typeof spec.engine === "string" ? spec.engine : null,
    repo: input.repo ?? null,
    domain: input.domain ?? null,
    version: "v1",
    lastDeployAt: now,
  });
  if (!cp) {
    // Demo-mode deploy timeline seed. CP mode never fabricates deployments —
    // the real pipeline rows come from the control plane.
    await db.insert(s.deployments).values({
      id: rid("dep"),
      resourceId: id,
      sha: sha7(),
      status: "running",
      author: "you",
      durationSec: 42,
      startedAt: now,
    });
  }
  // A domain is a first-class record on the control plane, not a field on the
  // spec: the reconciler routes from the `domains` rows, and nothing reads
  // spec.domain. The wizard collected one and wrote it only to the spec and the
  // local mirror, so in DEMO mode the dashboard showed the app reachable at it
  // while in CP mode it was silently discarded — the one direction of demo/CP
  // divergence that matters, on the screen that promises "Reachable at https://…".
  let domainError: string | undefined;
  if (cpEnabled() && input.domain?.trim()) {
    try {
      await attachDomain({ orgId: project.orgId, resourceId: id, domain: input.domain });
    } catch (err) {
      // The resource exists and deploys; only the hostname is missing. Say so
      // rather than failing a create the user would then have to redo — they
      // can attach it from the resource page.
      domainError = err instanceof Error ? err.message : "Could not attach the domain.";
    }
  }
  await writeAudit({ orgId: project.orgId, actor: user.name, action: "Created resource", target: `${name} · ${input.kind}` });
  revalidatePath("/dashboard", "layout");
  return { id, domainError };
}

/** Kick off a redeploy: a new deployment enters the pipeline as `queued`. */
export async function deployResource(input: { resourceId: string }) {
  const resource = await getResource(input.resourceId);
  if (!resource) throw new Error("Resource not found.");
  const project = await getProject(resource.projectId);
  if (!project) throw new Error("Resource not found.");
  const orgId = project.orgId;
  // Require effective Project Admin on the resource's project in BOTH modes
  // (SIGMA-114). Previously this gate lived only inside the cpEnabled branch, so
  // the demo fall-through queued a redeploy after nothing more than an org
  // membership check — a project-scoped user with no grant could drive it.
  //
  // The refusal is RETURNED, not thrown (SIGMA-308). It used to throw here,
  // outside the try/catch below that exists precisely so a CP refusal stays
  // readable — and Next.js redacts a thrown server-action message in
  // production, so a Developer pressing Deploy read "An error occurred in the
  // Server Components render…" plus a digest. The page no longer offers them
  // the button, but a direct invocation must still produce a sentence naming
  // the role they need rather than an opaque digest.
  let user: { name: string };
  let role: string;
  try {
    ({ user, role } = await requireProjectAdminForResource(orgId, input.resourceId));
  } catch (err) {
    return {
      error:
        err instanceof Error
          ? err.message
          : "You need the Project Admin role for this project to deploy.",
    };
  }
  // CP mode: queue a real manual redeploy (fresh clone→build→rollout). The CP
  // drives the pipeline status, so there's no client-side simulation to advance.
  if (cpEnabled()) {
    // A CP refusal (e.g. 422 "nothing to redeploy — connect a repo and push
    // first") must reach the user as-is: thrown server-action errors get
    // their messages redacted by Next.js in production, so return it instead.
    let dep;
    try {
      dep = await cpRedeploy(orgId, input.resourceId, { name: user.name, role });
    } catch (err) {
      return {
        error: err instanceof Error ? err.message : "Deploy failed. Please try again.",
      };
    }
    await writeAudit({ orgId, actor: user.name, action: "Redeployed resource", target: resource.name });
    revalidatePath("/dashboard", "layout");
    revalidatePath(`/dashboard/resources/${input.resourceId}`);
    // An empty id marks the forced re-apply path (db/s3/no-history resources):
    // no build pipeline — the agent re-runs the resource's ops instead.
    return { deploymentId: dep.id, cp: true as const, reapplied: !dep.id };
  }
  const id = rid("dep");
  const now = new Date();
  await db.insert(s.deployments).values({
    id,
    resourceId: input.resourceId,
    sha: sha7(),
    status: "queued",
    author: "you",
    durationSec: 0,
    startedAt: now,
  });
  await db
    .update(s.resources)
    .set({ lastDeployAt: now })
    .where(eq(s.resources.id, input.resourceId));
  // Audit only after the redeploy is actually enqueued, so a failed insert
  // can't leave a phantom "Redeployed" row (matches every sibling action).
  await writeAudit({ orgId, actor: user.name, action: "Redeployed resource", target: resource.name });
  revalidatePath("/dashboard", "layout");
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return { deploymentId: id };
}

/** Advance one deployment one step: queued → building → running. On reaching
 *  running it becomes the live build (older running builds → success). */
export async function advanceDeployment(input: { deploymentId: string }) {
  const [dep] = await db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.id, input.deploymentId));
  if (!dep) return { status: null };
  // Authorize on the resource's PROJECT, not bare org membership (SIGMA-110):
  // this drives the deploy state machine (supersedes running builds, flips the
  // resource to running), so a user scoped away from the resource's project must
  // not advance it. Mirrors the Project-Admin gate deployResource applies.
  const resource = await getResource(dep.resourceId);
  if (!resource) return { status: null };
  const project = await getProject(resource.projectId);
  if (!project) return { status: null };
  await requireProjectAdminForResource(project.orgId, dep.resourceId);

  let next: string;
  if (dep.status === "queued") next = "building";
  else if (dep.status === "building") next = "running";
  else return { status: dep.status };

  if (next === "running") {
    await db
      .update(s.deployments)
      .set({ status: "success" })
      .where(
        and(
          eq(s.deployments.resourceId, dep.resourceId),
          eq(s.deployments.status, "running"),
          ne(s.deployments.id, dep.id)
        )
      );
    await db
      .update(s.resources)
      .set({ status: "running" })
      .where(eq(s.resources.id, dep.resourceId));
  }
  await db
    .update(s.deployments)
    .set({ status: next, durationSec: next === "running" ? 47 : 12 })
    .where(eq(s.deployments.id, input.deploymentId));

  revalidatePath("/dashboard", "layout");
  revalidatePath(`/dashboard/resources/${dep.resourceId}`);
  return { status: next };
}

export async function deleteResource(input: { resourceId: string }) {
  const resource = await getResource(input.resourceId);
  if (!resource) return;
  const project = await getProject(resource.projectId);
  const membership = project ? await requireProjectRole(project.orgId, project.id, "Project Admin") : null;
  if (project && membership && cpEnabled()) {
    await cpDeleteResource(project.orgId, input.resourceId, {
      name: membership.user.name,
      role: membership.role,
    });
  }
  await db.delete(s.resources).where(eq(s.resources.id, input.resourceId));
  if (project && membership) {
    await writeAudit({ orgId: project.orgId, actor: membership.user.name, action: "Deleted resource", target: resource.name });
  }
  revalidatePath("/dashboard", "layout");
}

// ── Two-phase destructive-op confirm (P1-3) ─────────────────────────────────
// Deleting a named data volume is destructive and irreversible, so it takes an
// explicit two-phase approval: requestVolumeDeleteConfirm mints a short-lived
// confirm token from the control plane, the dialog presents it, and
// confirmVolumeDelete executes it. Both gate on Project Admin. The Docker volume
// name mirrors the CP's naming (sigmahub-<resourceId>-<name>).
const VOLUME_REMOVE_KIND = "volume.remove";
const dockerVolumeName = (resourceId: string, name: string) => `sigmahub-${resourceId}-${name}`;

async function volumeDeleteContext(resourceId: string) {
  const resource = await getResource(resourceId);
  if (!resource) throw new Error("Resource not found.");
  const project = await getProject(resource.projectId);
  if (!project) throw new Error("Resource not found.");
  const membership = await requireProjectRole(project.orgId, project.id, "Project Admin");
  if (!resource.serverId) throw new Error("Resource is not bound to a server.");
  return { resource, orgId: project.orgId, serverId: resource.serverId, membership };
}

export async function requestVolumeDeleteConfirm(input: { resourceId: string; volumeName: string }) {
  const { orgId, serverId, membership } = await volumeDeleteContext(input.resourceId);
  const target = dockerVolumeName(input.resourceId, input.volumeName);
  if (cpEnabled()) {
    const { token, expiresAt } = await cpRequestConfirmToken(
      orgId,
      { serverId, opKind: VOLUME_REMOVE_KIND, target },
      { name: membership.user.name, role: membership.role }
    );
    return { mode: "cp" as const, token, expiresAt };
  }
  // Demo mode: synthesise a token so the dialog still functions offline.
  return {
    mode: "sim" as const,
    token: `sim_${sha7()}`,
    expiresAt: new Date(Date.now() + 5 * 60 * 1000).toISOString(),
  };
}

export async function confirmVolumeDelete(input: { resourceId: string; volumeName: string; token: string }) {
  const { resource, orgId, serverId, membership } = await volumeDeleteContext(input.resourceId);
  const target = dockerVolumeName(input.resourceId, input.volumeName);
  if (cpEnabled()) {
    await cpConfirmDestructive(
      orgId,
      { serverId, token: input.token, opKind: VOLUME_REMOVE_KIND, target },
      { name: membership.user.name, role: membership.role }
    );
  }
  await writeAudit({
    orgId,
    actor: membership.user.name,
    action: "Deleted data volume",
    target: `${resource.name} · ${input.volumeName}`,
  });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
}
