// SigmaHub v1 data model — Drizzle (PostgreSQL dialect; canonical A-4 §5).
// Dev runs on embedded PGlite; prod = AlloyDB / Cloud SQL for PostgreSQL 18.
// NOTE: user auth (user/session/account/two_factor) is owned by better-auth in
// ./auth-schema (V1-2; prod = GCP Identity Platform). Orgs + memberships are the
// app's own tables and reference better-auth's `user`.

import {
  pgTable,
  text,
  integer,
  boolean,
  jsonb,
  timestamp,
  primaryKey,
  unique,
  uniqueIndex,
} from "drizzle-orm/pg-core";
import { sql } from "drizzle-orm";
import { user } from "./auth-schema";
import type { FailedRequirement, HostFacts } from "@/lib/server-compat";

export const orgs = pgTable("orgs", {
  id: text("id").primaryKey(),
  name: text("name").notNull(),
  slug: text("slug").notNull().unique(),
  plan: text("plan").notNull().default("free"), // free | cloud
  createdAt: timestamp("created_at").notNull().defaultNow(),
});

export const memberships = pgTable(
  "memberships",
  {
    id: text("id").primaryKey(),
    orgId: text("org_id")
      .notNull()
      .references(() => orgs.id, { onDelete: "cascade" }),
    userId: text("user_id")
      .notNull()
      .references(() => user.id, { onDelete: "cascade" }),
    role: text("role").notNull().default("Developer"), // Org Admin | Project Admin | Developer
    // SIGMA-167: explicit scoping state. Set true the FIRST time a project
    // grant is issued and never cleared implicitly. Previously "is this user
    // project-scoped?" was inferred from a live grant COUNT, so revoking a
    // contractor's last grant — or deleting the only project they were
    // granted — silently re-widened them to every project in the org, while
    // the toast and audit trail described a narrowing. A scoped member with
    // zero grants now sees NOTHING (fail closed); restoring org-wide access
    // is an explicit admin action that clears this flag.
    scoped: boolean("scoped").notNull().default(false),
    createdAt: timestamp("created_at").notNull().defaultNow(),
  },
  // At most one membership per (org, user) — the DB is the authority so a
  // concurrent acceptInvite race can't create duplicate rows with divergent
  // roles (SIGMA-111). Mirrors project_memberships.
  (t) => ({ uniq: unique().on(t.orgId, t.userId) })
);

// P2-7: per-project role grants. Semantics (enforced in active-org.ts):
// the org role is always the ceiling; a user with ZERO rows here keeps
// org-wide access to every project (backward compatible — nobody loses
// access when this ships); a user with ANY row becomes project-scoped and
// sees only the projects they are granted. Org Admins are never scoped.
export const projectMemberships = pgTable(
  "project_memberships",
  {
    id: text("id").primaryKey(),
    projectId: text("project_id")
      .notNull()
      .references(() => projects.id, { onDelete: "cascade" }),
    userId: text("user_id")
      .notNull()
      .references(() => user.id, { onDelete: "cascade" }),
    role: text("role").notNull().default("Developer"), // Project Admin | Developer
    createdAt: timestamp("created_at").notNull().defaultNow(),
  },
  (t) => ({ uniq: unique().on(t.projectId, t.userId) })
);

// P2-7b: pending email invitations. Replaces instant-join (which minted a
// display-only user row with no account — a member who could never log in).
// An invite carries the intended org role and optional per-project grants; the
// raw token lives only in the emailed link, we store its SHA-256 hash. On
// accept (by a real signed-in account whose email matches), the membership and
// grants are materialized and the token is one-time-invalidated.
export const invitations = pgTable(
  "invitations",
  {
    id: text("id").primaryKey(),
    orgId: text("org_id")
      .notNull()
      .references(() => orgs.id, { onDelete: "cascade" }),
    email: text("email").notNull(),
    role: text("role").notNull().default("Developer"), // Org Admin | Project Admin | Developer
    // Optional per-project grants to materialize on accept: JSON [{projectId, role}].
    projectGrants: text("project_grants").notNull().default("[]"),
    tokenHash: text("token_hash").notNull().unique(), // SHA-256 of the raw token
    invitedBy: text("invited_by").notNull(), // actor display name (audit-consistent)
    status: text("status").notNull().default("pending"), // pending | accepted | revoked
    expiresAt: timestamp("expires_at").notNull(),
    createdAt: timestamp("created_at").notNull().defaultNow(),
    acceptedAt: timestamp("accepted_at"),
  },
  // At most one PENDING invite per (org, lower(email)) — the DB is the authority
  // so a concurrent inviteMember race can't create two live invite links for the
  // same email, which would break the revoke workflow (SIGMA-115). Partial so
  // accepted/revoked rows don't collide.
  (t) => ({
    pendingEmailUniq: uniqueIndex("invitations_org_pending_email_uniq")
      .on(t.orgId, sql`lower(${t.email})`)
      .where(sql`${t.status} = 'pending'`),
  })
);

export const projects = pgTable("projects", {
  id: text("id").primaryKey(),
  orgId: text("org_id")
    .notNull()
    .references(() => orgs.id, { onDelete: "cascade" }),
  name: text("name").notNull(),
  slug: text("slug").notNull(),
  description: text("description").notNull().default(""),
  createdAt: timestamp("created_at").notNull().defaultNow(),
});

export const environments = pgTable("environments", {
  id: text("id").primaryKey(),
  projectId: text("project_id")
    .notNull()
    .references(() => projects.id, { onDelete: "cascade" }),
  name: text("name").notNull(),
  // SIGMA-190: mirrored from the CP so the dashboard can display and edit the
  // flag that seeds database backup retention (previously dropped by the
  // mirror, invisible, and write-once).
  production: boolean("production").notNull().default(false),
  createdAt: timestamp("created_at").notNull().defaultNow(),
});

export const servers = pgTable("servers", {
  id: text("id").primaryKey(),
  orgId: text("org_id")
    .notNull()
    .references(() => orgs.id, { onDelete: "cascade" }),
  name: text("name").notNull(),
  type: text("type").notNull(), // general | storage | database | gpu
  source: text("source").notNull().default("byo"), // byo | provider_integration
  provider: text("provider").notNull(),
  region: text("region").notNull(),
  status: text("status").notNull().default("provisioning"),
  agentVersion: text("agent_version").notNull().default(""),
  // ip is the server's PUBLIC address; meshIp is the private 10.8.x.x
  // WireGuard address. The UI previously presented the mesh IP under an "IP"
  // header because the public one never reached this table (SIGMA-187).
  ip: text("ip").notNull().default(""),
  meshIp: text("mesh_ip").notNull().default(""),
  cpu: integer("cpu").notNull().default(0),
  memGb: integer("mem_gb").notNull().default(0),
  byoVpn: boolean("byo_vpn").notNull().default(false),
  connectedAt: timestamp("connected_at").notNull().defaultNow(),
  // The agent's host description (SIGMA-201). In CP mode this mirrors what the
  // control plane stores; in demo mode the simulated check-in writes it. It is
  // what the detail page reads for arch/disk/GPU — figures the row's own cpu
  // and mem_gb columns never covered — and what the demo gate is evaluated
  // against, so a demo server's state comes from facts rather than from a
  // hardcoded status.
  facts: jsonb("facts").$type<HostFacts>().notNull().default(sql`'{}'::jsonb`),
  // Why a server is `incompatible` (SIGMA-203), rendered verbatim. Always an
  // array: a nullable column would have every reader deciding for itself what
  // null means, which is the distinction the facts column already taught us to
  // keep explicit.
  incompatibleReasons: jsonb("incompatible_reasons")
    .$type<FailedRequirement[]>()
    .notNull()
    .default(sql`'[]'::jsonb`),
  // The name was assigned by the product, not chosen by the operator — the
  // connect form stopped asking, so registration fills it from the reported
  // hostname while this is set (SIGMA-202).
  nameAuto: boolean("name_auto").notNull().default(false),
});

export const envServers = pgTable(
  "env_servers",
  {
    environmentId: text("environment_id")
      .notNull()
      .references(() => environments.id, { onDelete: "cascade" }),
    serverId: text("server_id")
      .notNull()
      .references(() => servers.id, { onDelete: "cascade" }),
  },
  (t) => ({ pk: primaryKey({ columns: [t.environmentId, t.serverId] }) })
);

export const resources = pgTable("resources", {
  id: text("id").primaryKey(),
  projectId: text("project_id")
    .notNull()
    .references(() => projects.id, { onDelete: "cascade" }),
  environmentId: text("environment_id")
    .notNull()
    .references(() => environments.id, { onDelete: "cascade" }),
  serverId: text("server_id").references(() => servers.id, {
    onDelete: "set null",
  }),
  name: text("name").notNull(),
  kind: text("kind").notNull(), // app | postgres | mysql | mongodb | redis | s3 | llm
  status: text("status").notNull().default("provisioning"),
  repo: text("repo"),
  domain: text("domain"),
  version: text("version"),
  // SIGMA-194: PR-preview resources, torn down with their PR. Badged in the
  // UI and their Delete carries an explicit preview-breaking warning.
  ephemeral: boolean("ephemeral").notNull().default(false),
  lastDeployAt: timestamp("last_deploy_at").notNull().defaultNow(),
});

export const deployments = pgTable("deployments", {
  id: text("id").primaryKey(),
  resourceId: text("resource_id")
    .notNull()
    .references(() => resources.id, { onDelete: "cascade" }),
  sha: text("sha").notNull(),
  status: text("status").notNull().default("queued"), // queued | building | running | success | failed
  author: text("author").notNull().default(""),
  durationSec: integer("duration_sec").notNull().default(0),
  startedAt: timestamp("started_at").notNull().defaultNow(),
});

export const auditLog = pgTable("audit_log", {
  id: text("id").primaryKey(),
  orgId: text("org_id").notNull(),
  actor: text("actor").notNull(),
  action: text("action").notNull(),
  target: text("target").notNull().default(""),
  createdAt: timestamp("created_at").notNull().defaultNow(),
});
