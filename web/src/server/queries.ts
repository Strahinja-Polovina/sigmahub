import "server-only";
import { eq } from "drizzle-orm";
import { db } from "./db";
import * as s from "./db/schema";

export const UNIT_PRICE = 5;
export const FREE_TIER_SERVERS = 3;
export const CURRENCY = "EUR";

export async function getOrgs() {
  return db.select().from(s.orgs);
}
export async function getOrg(id: string) {
  return (await db.select().from(s.orgs).where(eq(s.orgs.id, id)))[0];
}
export async function getProjects(orgId: string) {
  return db.select().from(s.projects).where(eq(s.projects.orgId, orgId));
}
export async function getProject(id: string) {
  return (await db.select().from(s.projects).where(eq(s.projects.id, id)))[0];
}
export async function getEnvironments(projectId: string) {
  return db
    .select()
    .from(s.environments)
    .where(eq(s.environments.projectId, projectId));
}
export async function getEnvironment(id: string) {
  return (
    await db.select().from(s.environments).where(eq(s.environments.id, id))
  )[0];
}
export async function getServers(orgId: string) {
  return db.select().from(s.servers).where(eq(s.servers.orgId, orgId));
}
export async function getServer(id: string) {
  return (await db.select().from(s.servers).where(eq(s.servers.id, id)))[0];
}
export async function getResources(environmentId: string) {
  return db
    .select()
    .from(s.resources)
    .where(eq(s.resources.environmentId, environmentId));
}
export async function getResourcesByProject(projectId: string) {
  return db
    .select()
    .from(s.resources)
    .where(eq(s.resources.projectId, projectId));
}
export async function getResource(id: string) {
  return (await db.select().from(s.resources).where(eq(s.resources.id, id)))[0];
}
export async function getMembers(orgId: string) {
  return db
    .select({
      id: s.users.id,
      name: s.users.name,
      email: s.users.email,
      role: s.memberships.role,
    })
    .from(s.memberships)
    .innerJoin(s.users, eq(s.memberships.userId, s.users.id))
    .where(eq(s.memberships.orgId, orgId));
}
export async function getDeployments(resourceId: string) {
  return db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.resourceId, resourceId));
}
export async function getBillingSummary(orgId: string) {
  const all = await db.select().from(s.servers).where(eq(s.servers.orgId, orgId));
  const connected = all.filter((x) => x.status !== "provisioning").length;
  const isFree = connected <= FREE_TIER_SERVERS;
  return {
    connected,
    freeTier: FREE_TIER_SERVERS,
    unitPrice: UNIT_PRICE,
    currency: CURRENCY,
    amount: isFree ? 0 : connected * UNIT_PRICE,
    isFree,
  };
}
