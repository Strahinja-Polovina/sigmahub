// Applies migrations to the (PGlite) DB and seeds it with the canonical demo
// dataset — mirrors the old mock so the app looks identical on first run.
// Run: pnpm db:seed
import { migrate } from "drizzle-orm/pglite/migrator";
import { db, client } from "./index";
import * as s from "./schema";
import {
  orgs as mockOrgs,
  projects as mockProjects,
  environments as mockEnvs,
  servers as mockServers,
  resources as mockResources,
  members as mockMembers,
} from "../../lib/mock/data";

function sha7(x: string) {
  let h = 5381;
  for (const c of x) h = ((h << 5) + h + c.charCodeAt(0)) >>> 0;
  return h.toString(16).padStart(8, "0").slice(0, 7);
}

async function main() {
  await migrate(db, { migrationsFolder: "drizzle" });

  // Idempotent: clear (child → parent order) then insert.
  await db.delete(s.deployments);
  await db.delete(s.resources);
  await db.delete(s.envServers);
  await db.delete(s.servers);
  await db.delete(s.environments);
  await db.delete(s.projects);
  await db.delete(s.memberships);
  await db.delete(s.users);
  await db.delete(s.orgs);

  await db.insert(s.orgs).values(
    mockOrgs.map((o) => ({ id: o.id, name: o.name, slug: o.slug, plan: o.plan }))
  );

  await db.insert(s.users).values(
    mockMembers.map((m) => ({ id: m.id, name: m.name, email: m.email }))
  );
  await db.insert(s.memberships).values([
    ...mockMembers.map((m) => ({
      id: `mem_acme_${m.id}`,
      orgId: "org_acme",
      userId: m.id,
      role: m.role,
    })),
    // demo user is also Org Admin of Northwind so both orgs have a member
    { id: "mem_nw_u1", orgId: "org_northwind", userId: "u1", role: "Org Admin" },
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

  await db.insert(s.servers).values(
    mockServers.map((sv) => ({
      id: sv.id,
      orgId: sv.orgId,
      name: sv.name,
      type: sv.type,
      source: "byo",
      provider: sv.provider,
      region: sv.region,
      status: sv.status,
      agentVersion: sv.agentVersion,
      ip: sv.ip,
      cpu: sv.cpu,
      memGb: sv.memGb,
      byoVpn: sv.byoVpn,
      connectedAt: new Date(sv.connectedAt),
    }))
  );

  await db.insert(s.envServers).values(
    mockEnvs.flatMap((e) =>
      e.serverIds.map((serverId) => ({ environmentId: e.id, serverId }))
    )
  );

  await db.insert(s.resources).values(
    mockResources.map((r) => ({
      id: r.id,
      projectId: r.projectId,
      environmentId: r.environmentId,
      serverId: r.serverId ?? null,
      name: r.name,
      kind: r.kind,
      status: r.status,
      repo: r.repo ?? null,
      domain: r.domain ?? null,
      version: r.version ?? null,
      lastDeployAt: new Date(r.lastDeployAt),
    }))
  );

  await db.insert(s.deployments).values(
    mockResources.flatMap((r) =>
      [0, 1, 2].map((i) => ({
        id: `dep_${r.id}_${i}`,
        resourceId: r.id,
        sha: sha7(r.id + i),
        status: i === 0 ? "running" : i === 1 ? "success" : "success",
        author: ["mila", "nikola", "ana"][i % 3],
        durationSec: 40 + i * 13,
        startedAt: new Date(Date.parse(r.lastDeployAt) - i * 86_400_000),
      }))
    )
  );

  const [orgN, projN, srvN, resN] = await Promise.all([
    db.$count(s.orgs),
    db.$count(s.projects),
    db.$count(s.servers),
    db.$count(s.resources),
  ]);
  console.log(
    `seed done — orgs:${orgN} projects:${projN} servers:${srvN} resources:${resN}`
  );
  await client.close();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
