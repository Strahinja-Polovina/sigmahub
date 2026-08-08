"use server";

import { revalidatePath } from "next/cache";
import { and, eq, ne } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireProjectAdminForResource, requireProjectRole } from "../active-org";
import { getProject, getResource } from "../queries";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpCreateResource,
  cpDeleteResource,
  cpKind,
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
    healthCheck?: { type: string; path?: string; port?: number };
  };
  /** Inference runtime + model, for kind "llm". The control plane refuses an
   *  unknown runtime, so this is passed through rather than defaulted. */
  llm?: { engine: string; model: string };
}) {
  const project = await getProject(input.projectId);
  if (!project) throw new Error("Project not found.");
  const { user, role } = await requireProjectRole(project.orgId, input.projectId, "Project Admin");
  const name = input.name.trim();
  if (!name) throw new Error("Resource name is required.");

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
  if (input.serverId && !cpEnabled()) {
    const [sv] = await db
      .select({ orgId: s.servers.orgId })
      .from(s.servers)
      .where(eq(s.servers.id, input.serverId));
    if (!sv || sv.orgId !== project.orgId) {
      throw new Error("Server does not belong to this organization.");
    }
  }

  // Build the persisted spec from what the inspector detected. `ports` drives
  // the rollout's exposed ports AND the default TCP health probe; `healthCheck`
  // overrides the probe when the repo declares one.
  const detectedPorts = (input.detected?.ports ?? []).filter((p) => Number.isInteger(p) && p > 0 && p < 65536);
  const spec: Record<string, unknown> = {
    repo: input.repo ?? null,
    domain: input.domain ?? null,
  };
  if (detectedPorts.length > 0) {
    spec.ports = detectedPorts.map((container) => ({ container, host: 0, protocol: "tcp" }));
  }
  if (input.detected?.healthCheck?.type) {
    spec.healthCheck = input.detected.healthCheck;
  }
  // An inference endpoint is defined by what it runs: without these the control
  // plane has nothing to render and refuses the create.
  if (input.llm?.engine) {
    spec.engine = input.llm.engine;
    spec.model = input.llm.model;
  }

  let id = rid("res");
  if (cpEnabled()) {
    // CP mode: the control plane owns the resource record and enforces the
    // kind/server-type availability matrix + env attachment server-side. The
    // local row mirrors it under the same id for read models; mirror the CP
    // server first so the local resources.server_id FK holds.
    if (!input.serverId) throw new Error("A target server is required.");
    await cpMirrorServer(project.orgId, input.serverId);
    const created = await cpCreateResource(
      project.orgId,
      {
        environmentId: input.environmentId,
        serverId: input.serverId,
        name,
        kind: cpKind(input.kind),
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
    serverId: input.serverId ?? null,
    name,
    kind: input.kind,
    status: cp ? "provisioning" : "running",
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
  await writeAudit({ orgId: project.orgId, actor: user.name, action: "Created resource", target: `${name} · ${input.kind}` });
  revalidatePath("/dashboard", "layout");
  return { id };
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
  const { user, role } = await requireProjectAdminForResource(orgId, input.resourceId);
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
