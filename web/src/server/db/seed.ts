// Applies migrations to the (PGlite) DB and seeds the canonical demo dataset —
// mirrors the old mock so the app looks identical on first run.
// Identity: the demo user is created through better-auth (real hashed password);
// the other members are display-only rows in better-auth's `user` table.
// Demo login → strahinja@sigmajunction.com / sigmahub123
// Run: pnpm db:seed
import { fileURLToPath } from "node:url";
import { migrate } from "drizzle-orm/pglite/migrator";
import { db, client } from "./index";
import * as s from "./schema";
import { user, session, account, verification, twoFactor } from "./auth-schema";
import { auth } from "../../lib/auth";
import { checkServerCompatibility, SERVER_STATUS } from "../../lib/server-compat";
import {
  orgs as mockOrgs,
  projects as mockProjects,
  environments as mockEnvs,
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

async function main() {
  await migrate(db, { migrationsFolder: "drizzle" });

  // Idempotent: clear (child → parent) then identity tables.
  await db.delete(s.deployments);
  await db.delete(s.resources);
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

  await db.insert(s.servers).values(
    mockServers.map((sv) => {
      // A seeded server's compatibility is DERIVED, never written down: the
      // gate runs over whatever facts the fixture reports, exactly as it does
      // at registration. A demo host filed under a type its facts contradict
      // therefore lands in `incompatible` with the control plane's own
      // sentences, and a fixture edit that fixes the hardware clears it —
      // neither is a status somebody remembered to update (SIGMA-203).
      const facts = sv.facts ?? {};
      const incompatibleReasons = checkServerCompatibility(sv.type, facts);
      return {
        id: sv.id,
        orgId: sv.orgId,
        name: sv.name,
        type: sv.type,
        source: "byo",
        provider: sv.provider,
        region: sv.region,
        status: incompatibleReasons.length > 0 ? SERVER_STATUS.incompatible : sv.status,
        agentVersion: sv.agentVersion,
        ip: sv.ip,
        cpu: sv.cpu,
        memGb: sv.memGb,
        byoVpn: sv.byoVpn,
        connectedAt: new Date(sv.connectedAt),
        facts,
        incompatibleReasons,
      };
    })
  );

  await db.insert(s.envServers).values(
    mockEnvs.flatMap((e) =>
      e.serverIds.map((serverId) => ({ environmentId: e.id, serverId }))
    )
  );

  // Deploy dates are relative to "now" (recent past) so that live deploys
  // created in-app sort as the newest — the mock's fixed 2027 dates would
  // otherwise always outrank them.
  const DAY = 86_400_000;
  const resourceDeployAt = (idx: number) =>
    new Date(Date.now() - ((idx % 12) + 1) * DAY - idx * 3_600_000);

  await db.insert(s.resources).values(
    mockResources.map((r, idx) => ({
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

  const [orgN, projN, srvN, resN] = await Promise.all([
    db.$count(s.orgs),
    db.$count(s.projects),
    db.$count(s.servers),
    db.$count(s.resources),
  ]);
  console.log(
    `seed done — orgs:${orgN} projects:${projN} servers:${srvN} resources:${resN} · ` +
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
