// SIGMA-325's cp-sync coverage, kept separate from cp-sync.test.ts.
//
// SIGMA-327 wrote a cp-sync.test.ts in the same wave to count the statements a
// mirror pass issues; this file covers the tenant-scoped deletes. They mock the
// same modules with different factories, so one file would mean one factory
// silently winning — see the note in project-members-scope.test.ts.

// What syncOrgMirror is allowed to delete (SIGMA-325).
//
// This module is the only place in the dashboard that removes mirror rows it
// did not just create, and it removes them from a diff: everything the local
// database holds, minus everything the control plane just listed for ONE org.
// That subtraction is only a tombstone list because the "everything local" side
// is org-scoped — the `innerJoin(projects)` filters in syncOrgMirror say so in
// a comment ("Scoping by org here is what makes the deletes below safe"), and
// the deletes themselves are keyed on nothing but the id list.
//
// Nothing tested it. Removing both innerJoins — the exact shape of a refactor
// that "simplifies" two selects — leaves all 741 web tests green while turning
// the tombstone step into a cross-tenant delete: every render of org A's
// dashboard wipes org B's environments and resources out of the mirror, and org
// B's users watch their projects empty out with no error anywhere.
//
// So this runs the real reconcile against the real (PGlite) schema with two
// orgs in it, and asserts the blast radius rather than the happy path.

import { beforeEach, describe, expect, it, vi } from "vitest";

import * as s from "@/server/db/schema";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";

/** What the control plane answers with, per test. Only org A is ever listed:
 *  a sync pass is for one org, and the point of the test is what happens to
 *  the org that was NOT listed. */
const cp = vi.hoisted(() => ({
  servers: [] as { id: string; orgId: string; name: string; type: string }[],
  projects: [] as { id: string; orgId: string; name: string; description: string }[],
  environments: [] as { id: string; projectId: string; name: string; production: boolean }[],
  resources: [] as {
    id: string;
    projectId: string;
    environmentId: string;
    serverId: string;
    name: string;
    kind: string;
  }[],
  envServerIds: {} as Record<string, string[]>,
}));

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});

vi.mock("@/server/cp", () => {
  const at = "2026-01-01T00:00:00.000Z";
  return {
    cpEnabled: () => true,
    // SIGMA-331 added a cluster mirror to the same pass these tests drive. An
    // unmocked export fails every test here at import, with a message about the
    // mock rather than about the tenant scoping under test.
    cpListClusters: async () => ({ clusters: [] }),
    cpListServers: async (orgId: string) => cp.servers.filter((x) => x.orgId === orgId),
    cpListProjects: async (orgId: string) =>
      cp.projects
        .filter((p) => p.orgId === orgId)
        .map((p) => ({ ...p, createdBy: "usr_you", createdAt: at })),
    cpListEnvironments: async (orgId: string, projectId: string) =>
      cp.environments
        .filter((e) => e.projectId === projectId)
        .map((e) => ({ ...e, orgId, createdAt: at })),
    cpListResources: async (orgId: string) =>
      cp.resources.map((r) => ({
        ...r,
        orgId,
        spec: {},
        status: {},
        createdAt: at,
        updatedAt: at,
      })),
    cpListOrgDeployments: async () => ({ recent: [], latest: [] }),
    cpEnvServerIds: async (_orgId: string, envId: string) => cp.envServerIds[envId] ?? [],
    // The real mapper's job is field translation, which is not what is under
    // test here; the mirror row just has to be insertable.
    cpServerToRow: (x: { id: string; orgId: string; name: string; type: string }) => ({
      id: x.id,
      orgId: x.orgId,
      name: x.name,
      type: x.type,
      source: "byo",
      provider: "BYO",
      region: "—",
      status: "running",
      agentVersion: "0.1.0",
      ip: "",
      meshIp: "",
      cpu: 0,
      memGb: 0,
      byoVpn: false,
      connectedAt: new Date(at),
      facts: {},
      incompatibleReasons: [],
      nameAuto: false,
      decommissionStartedAt: null,
      decommissionPurgeVolumes: false,
    }),
  };
});

const { db } = await import("@/server/db");
const { syncOrgMirror } = await import("@/server/cp-sync");

/** Org B: a second tenant with its own project, environment and resource, none
 *  of which the control plane will mention when org A is synced. */
const RIVAL = {
  projectId: "proj_rival",
  envId: "env_rival_prod",
  resourceId: "res_rival_api",
} as const;

/** Org A's second environment/resource — present locally, deliberately omitted
 *  from the CP's answer, so a correct pass DOES delete something. Without a
 *  real tombstone in the same pass, a sweep that deleted nothing at all would
 *  also satisfy the cross-tenant assertions. */
const GONE = {
  envId: FIXTURE.stagingEnvId,
  resourceId: "res_shop_old",
} as const;

const KEPT = { envId: FIXTURE.prodEnvId, resourceId: "res_shop_web" } as const;

beforeEach(async () => {
  const d = db as unknown as DemoDb;
  for (const t of [s.resources, s.envServers, s.environments, s.projects, s.servers, s.orgs]) {
    await d.delete(t);
  }
  await seedDemoFixture(d);

  await d.insert(s.projects).values({
    id: RIVAL.projectId,
    orgId: FIXTURE.rivalOrgId,
    name: "Rival App",
    slug: "rival-app",
  });
  await d.insert(s.environments).values({
    id: RIVAL.envId,
    projectId: RIVAL.projectId,
    name: "production",
    production: true,
  });
  await d.insert(s.resources).values([
    {
      id: RIVAL.resourceId,
      projectId: RIVAL.projectId,
      environmentId: RIVAL.envId,
      name: "api",
      kind: "app",
    },
    {
      id: KEPT.resourceId,
      projectId: FIXTURE.projectId,
      environmentId: KEPT.envId,
      name: "web",
      kind: "app",
    },
    {
      id: GONE.resourceId,
      projectId: FIXTURE.projectId,
      environmentId: GONE.envId,
      name: "old-worker",
      kind: "app",
    },
  ]);

  // The CP's view of org A: the project, production only, and the web
  // resource. Staging and old-worker are gone as far as it is concerned.
  cp.servers = [
    { id: FIXTURE.k8sHostIds[0], orgId: FIXTURE.orgId, name: "k8s-1", type: "k8s" },
  ];
  cp.projects = [
    { id: FIXTURE.projectId, orgId: FIXTURE.orgId, name: "Shop", description: "" },
  ];
  cp.environments = [
    { id: KEPT.envId, projectId: FIXTURE.projectId, name: "production", production: true },
  ];
  cp.resources = [
    {
      id: KEPT.resourceId,
      projectId: FIXTURE.projectId,
      environmentId: KEPT.envId,
      serverId: FIXTURE.k8sHostIds[0],
      name: "web",
      kind: "app",
    },
  ];
  cp.envServerIds = { [KEPT.envId]: [FIXTURE.k8sHostIds[0]] };
});

const ids = async (table: typeof s.environments | typeof s.resources) =>
  (await (db as unknown as DemoDb).select().from(table)).map((r) => r.id as string);

describe("syncOrgMirror", () => {
  it("syncing org A does not delete org B's environments or resources", async () => {
    await syncOrgMirror(FIXTURE.orgId);

    const envIds = await ids(s.environments);
    const resourceIds = await ids(s.resources);

    // The whole point: another tenant's rows are not part of this org's diff.
    expect(envIds).toContain(RIVAL.envId);
    expect(resourceIds).toContain(RIVAL.resourceId);
    // …and neither is its project, which would cascade both away.
    const projectIds = (
      await (db as unknown as DemoDb).select().from(s.projects)
    ).map((p) => p.id);
    expect(projectIds).toContain(RIVAL.projectId);
  });

  it("tombstones only the ids the control plane omitted for this org", async () => {
    await syncOrgMirror(FIXTURE.orgId);

    const envIds = await ids(s.environments);
    const resourceIds = await ids(s.resources);

    // Omitted by the CP → tombstoned. (Assert it, or the cross-tenant test
    // above would also pass against a sync that deleted nothing.)
    expect(envIds).not.toContain(GONE.envId);
    expect(resourceIds).not.toContain(GONE.resourceId);
    // Still owned by the CP → kept.
    expect(envIds).toContain(KEPT.envId);
    expect(resourceIds).toContain(KEPT.resourceId);
  });

  it("tombstones servers only within the synced org", async () => {
    // Every host in FIXTURE belongs to org A except the rival's, and the CP
    // now lists exactly one of org A's — the rest are stale.
    await syncOrgMirror(FIXTURE.orgId);

    const rows = await (db as unknown as DemoDb).select().from(s.servers);
    const byId = new Set(rows.map((r) => r.id));
    expect(byId).toContain(FIXTURE.k8sHostIds[0]);
    expect(byId).toContain(FIXTURE.rivalHostId); // another org's host: untouched
    expect(byId).not.toContain(FIXTURE.k8sHostIds[1]); // org A's, no longer listed
    expect(byId).not.toContain(FIXTURE.dbHostId);
  });

  it("upserts CP-created environments the mirror has never seen", async () => {
    // A preview environment (pr-<n>) is created by the control plane and has no
    // local row — making them visible is the other half of this module's job,
    // and a pass that only ever deleted would still satisfy the tests above.
    cp.environments.push({
      id: "env_shop_pr_42",
      projectId: FIXTURE.projectId,
      name: "pr-42",
      production: false,
    });
    await syncOrgMirror(FIXTURE.orgId);
    expect(await ids(s.environments)).toContain("env_shop_pr_42");
  });
});
