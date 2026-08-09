// Applies migrations to the (PGlite) DB and seeds the canonical demo dataset —
// mirrors the old mock so the app looks identical on first run.
// Identity: the demo user is created through better-auth (real hashed password);
// the other members are display-only rows in better-auth's `user` table.
// Demo login → strahinja@sigmajunction.com / sigmahub123
// Run: pnpm db:seed
import { fileURLToPath } from "node:url";
import { sql } from "drizzle-orm";
import { migrate } from "drizzle-orm/pglite/migrator";
import { db, client } from "./index";
import { repairMigrationLedger } from "./migrate-repair";
import * as s from "./schema";
import { user, session, account, verification, twoFactor } from "./auth-schema";
import { auth } from "../../lib/auth";
import { checkServerCompatibility, SERVER_STATUS } from "../../lib/server-compat";
import { clusterCanHost } from "../../lib/server-catalog.generated";
import {
  CLUSTER_STATUS,
  demoApiEndpoint,
  demoClusterStatus,
  demoKubernetesVersion,
  demoNodeReport,
} from "../../lib/demo-cluster";
import {
  orgs as mockOrgs,
  projects as mockProjects,
  environments as mockEnvs,
  clusters as mockClusters,
  servers as mockServers,
  resources as mockResources,
  members as mockMembers,
} from "../../lib/mock/data";

export const DEMO_EMAIL = "strahinja@sigmajunction.com";
export const DEMO_PASSWORD = "sigmahub123";

function sha7(x: string) {
  let h = 5381;
  for (const c of x) h = ((h << 5) + h + c.charCodeAt(0)) >>> 0;
  return h.toString(16).padStart(8, "0").slice(0, 7);
}

/** Refuse to seed a fixture the product itself would not have accepted.
 *
 *  Demo data is the first thing a prospective user sees, so a row the wizard
 *  could never have produced is worse than a missing one: it teaches a rule and
 *  then breaks it on the next screen. Both checks below are the control plane's
 *  own — one target per resource (its migration 0050's CHECK constraint), and
 *  the kinds a cluster refuses, read from the generated catalog rather than
 *  listed here so a change on the Go side lands in this guard for free. */
function assertResourceTargetsAreLegal() {
  for (const r of mockResources) {
    const targets = [r.serverId, r.clusterId].filter(Boolean).length;
    if (targets !== 1) {
      throw new Error(
        `Demo resource ${r.name} has ${targets} deploy targets. Set exactly one of serverId or clusterId on it in web/src/lib/mock/data.ts.`
      );
    }
    if (r.clusterId && !clusterCanHost(r.kind)) {
      throw new Error(
        `Demo resource ${r.name} is seeded into a cluster, but the control plane will not schedule a ${r.kind} inside one. Give it a serverId instead in web/src/lib/mock/data.ts.`
      );
    }
  }
}

async function main() {
  // Before the migrator, never after: it reads a high-water mark that one
  // journal entry poisoned, and the demo's PGlite directory persists across
  // runs, so a developer's local copy carries the same bad row a server does.
  // See migrate-repair.ts.
  await repairMigrationLedger((stmt) => db.execute(sql.raw(stmt)));
  await migrate(db, { migrationsFolder: "drizzle" });

  // Before a single row is written, so a bad fixture fails at the top with a
  // sentence naming the file to fix rather than as a foreign-key error two
  // hundred inserts later.
  assertResourceTargetsAreLegal();

  // Idempotent: clear (child → parent) then identity tables.
  await db.delete(s.deployments);
  await db.delete(s.resources);
  await db.delete(s.clusterNodes);
  await db.delete(s.clusters);
  await db.delete(s.envServers);
  await db.delete(s.servers);
  await db.delete(s.environments);
  await db.delete(s.projects);
  await db.delete(s.memberships);
  await db.delete(s.orgs);
  await db.delete(s.auditLog);
  await db.delete(twoFactor);
  await db.delete(session);
  await db.delete(account);
  await db.delete(verification);
  await db.delete(user);

  // Demo user via better-auth → creates `user` + `account` (hashed password).
  const demo = mockMembers[0];
  const signUp = await auth.api.signUpEmail({
    body: { email: DEMO_EMAIL, password: DEMO_PASSWORD, name: demo.name },
  });
  const demoUserId = signUp.user.id;

  // Other members are display-only (no credentials) → direct user rows.
  const others = mockMembers.slice(1);
  await db.insert(user).values(
    others.map((m) => ({
      id: m.id,
      name: m.name,
      email: m.email,
      emailVerified: true,
    }))
  );

  await db.insert(s.orgs).values(
    mockOrgs.map((o) => ({ id: o.id, name: o.name, slug: o.slug, plan: o.plan }))
  );

  await db.insert(s.memberships).values([
    { id: "mem_acme_u1", orgId: "org_acme", userId: demoUserId, role: demo.role },
    ...others.map((m) => ({
      id: `mem_acme_${m.id}`,
      orgId: "org_acme",
      userId: m.id,
      role: m.role,
    })),
    // demo user is also Org Admin of Northwind so both orgs have a member
    { id: "mem_nw_u1", orgId: "org_northwind", userId: demoUserId, role: "Org Admin" },
  ]);

  await db.insert(s.projects).values(
    mockProjects.map((p) => ({
      id: p.id,
      orgId: p.orgId,
      name: p.name,
      slug: p.slug,
      description: p.description,
    }))
  );

  await db.insert(s.environments).values(
    mockEnvs.map((e) => ({ id: e.id, projectId: e.projectId, name: e.name }))
  );

  // Every clock the demo hands to a live comparison is measured from ONE
  // instant, so a seed run cannot produce a fleet whose in-flight states were
  // each captured a few milliseconds apart.
  const seededAt = Date.now();

  const DAY = 86_400_000;

  // Kept as a value rather than inlined into the insert: the cluster rows below
  // are derived from the status each of these hosts LANDED in, which is not the
  // status its fixture states — and reading it back out of the database would be
  // a round trip to learn something we just decided.
  const serverRows = mockServers.map((sv) => {
    // A seeded server's compatibility is DERIVED, never written down: the gate
    // runs over whatever facts the fixture reports, exactly as it does at
    // registration. A demo host filed under a type its facts contradict
    // therefore lands in `incompatible` with the control plane's own sentences,
    // and a fixture edit that fixes the hardware clears it — neither is a status
    // somebody remembered to update (SIGMA-203).
    const facts = sv.facts ?? {};
    const incompatibleReasons = checkServerCompatibility(sv.type, facts);
    // The teardown clock, likewise derived — the fixture states an OFFSET and
    // this is where it becomes a date, so "how far into the ten-minute window is
    // this demo" is answered from when the database was seeded rather than from
    // a calendar date somebody typed (SIGMA-204).
    const decommissionStartedAt = sv.decommission
      ? new Date(seededAt - sv.decommission.startedMinutesAgo * 60_000)
      : null;
    return {
      id: sv.id,
      orgId: sv.orgId,
      name: sv.name,
      type: sv.type,
      source: "byo",
      provider: sv.provider,
      region: sv.region,
      // `decommissioning` outranks the gate, which is the same terminal rule
      // nextServerStatus holds and load-bearing for the same reason: one of the
      // two documented exits from `incompatible` IS disconnecting, so the host
      // most likely to be torn down is one whose facts fail its type.
      status: decommissionStartedAt
        ? SERVER_STATUS.decommissioning
        : incompatibleReasons.length > 0
          ? SERVER_STATUS.incompatible
          : sv.status,
      agentVersion: sv.agentVersion,
      ip: sv.ip,
      meshIp: sv.meshIp,
      cpu: sv.cpu,
      memGb: sv.memGb,
      byoVpn: sv.byoVpn,
      connectedAt: new Date(sv.connectedAt),
      facts,
      incompatibleReasons,
      decommissionStartedAt,
      decommissionPurgeVolumes: sv.decommission?.purgeVolumes ?? false,
    };
  });
  await db.insert(s.servers).values(serverRows);

  await db.insert(s.envServers).values(
    mockEnvs.flatMap((e) =>
      e.serverIds.map((serverId) => ({ environmentId: e.id, serverId }))
    )
  );

  // A cluster's rows are DERIVED from its nodes' hosts, by the demo's own
  // functions rather than by a second copy of the rule here.
  //
  // The listing re-derives all of this on every read — a node's report comes
  // from the HOST's status and how long ago it joined (demoNodeReport), and the
  // cluster's status from the reports (demoClusterStatus, the TypeScript half of
  // store.rederiveClusterStatusTx). So these columns are not what the dashboard
  // will show; they are what it will show, written down. A seeded `error` node
  // over a running host would be corrected on the first render, and a stored
  // status that disagreed with the panel would send whoever next opened this
  // database looking for a bug that is not there.
  const seededServer = new Map(serverRows.map((row) => [row.id, row]));
  const seededClusters = mockClusters.map((c) => {
    const nodes = c.nodes.map((n) => {
      const joinedAt = new Date(seededAt - n.joinedDaysAgo * DAY);
      const report = demoNodeReport({
        joinedAt,
        serverStatus: seededServer.get(n.serverId)?.status ?? SERVER_STATUS.provisioning,
        now: seededAt,
      });
      return { node: n, joinedAt, report };
    });
    const status = demoClusterStatus(
      nodes.map(({ node, report }) => ({ role: node.role, status: report.status }))
    );
    const controlPlane = nodes.find(({ node }) => node.role === "control-plane");
    return { cluster: c, nodes, status, controlPlaneId: controlPlane?.node.serverId };
  });

  await db.insert(s.clusters).values(
    seededClusters.map(({ cluster, status, controlPlaneId }) => ({
      id: cluster.id,
      orgId: cluster.orgId,
      environmentId: cluster.environmentId,
      name: cluster.name,
      status,
      // Empty while provisioning: the API server is not answering yet, and a
      // placeholder URL there is an address someone can try to curl.
      apiEndpoint:
        status === CLUSTER_STATUS.provisioning
          ? ""
          : demoApiEndpoint(controlPlaneId ? seededServer.get(controlPlaneId)?.meshIp : ""),
      kubernetesVersion: demoKubernetesVersion(status),
      createdBy: cluster.createdBy,
      createdAt: new Date(seededAt - cluster.createdDaysAgo * DAY),
    }))
  );

  await db.insert(s.clusterNodes).values(
    seededClusters.flatMap(({ cluster, nodes }) =>
      nodes.map(({ node, joinedAt, report }) => ({
        clusterId: cluster.id,
        serverId: node.serverId,
        role: node.role,
        nodeStatus: report.status,
        nodeMessage: report.message,
        joinedAt,
        // A node still `pending` has said nothing about Kubernetes yet, and a
        // timestamp there would date a report that does not exist.
        reportedAt: report.status === "pending" ? null : new Date(seededAt),
      }))
    )
  );

  // Deploy dates are relative to "now" (recent past) so that live deploys
  // created in-app sort as the newest — the mock's fixed 2027 dates would
  // otherwise always outrank them.
  const resourceDeployAt = (idx: number) =>
    new Date(seededAt - ((idx % 12) + 1) * DAY - idx * 3_600_000);

  await db.insert(s.resources).values(
    mockResources.map((r, idx) => ({
      id: r.id,
      projectId: r.projectId,
      environmentId: r.environmentId,
      serverId: r.serverId,
      clusterId: r.clusterId ?? null,
      name: r.name,
      kind: r.kind,
      status: r.status,
      repo: r.repo ?? null,
      domain: r.domain ?? null,
      version: r.version ?? null,
      lastDeployAt: resourceDeployAt(idx),
    }))
  );

  await db.insert(s.deployments).values(
    mockResources.flatMap((r, idx) => {
      const base = resourceDeployAt(idx).getTime();
      return [0, 1, 2].map((i) => ({
        id: `dep_${r.id}_${i}`,
        resourceId: r.id,
        sha: sha7(r.id + i),
        status: i === 0 ? "running" : "success",
        author: ["mila", "nikola", "ana"][i % 3],
        durationSec: 40 + i * 13,
        startedAt: new Date(base - i * DAY),
      }));
    })
  );

  const [orgN, projN, srvN, clsN, resN] = await Promise.all([
    db.$count(s.orgs),
    db.$count(s.projects),
    db.$count(s.servers),
    db.$count(s.clusters),
    db.$count(s.resources),
  ]);
  console.log(
    `seed done — orgs:${orgN} projects:${projN} servers:${srvN} clusters:${clsN} resources:${resN} · ` +
      `login ${DEMO_EMAIL} / ${DEMO_PASSWORD}`
  );
  await client.close();
}

// Only run when executed directly (pnpm db:seed) — NOT when imported, so pulling
// in DEMO_EMAIL/DEMO_PASSWORD elsewhere doesn't wipe + reseed the database.
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((e) => {
    console.error(e);
    process.exit(1);
  });
}
