// SigmaHub v1 data model — Drizzle (PostgreSQL dialect; canonical A-4 §5).
// Dev runs on embedded PGlite; prod = AlloyDB / Cloud SQL for PostgreSQL 18.
// NOTE: user/org/membership here are the app's minimal identity tables so the
// app runs end-to-end; V1-2 layers GCP Identity Platform / better-auth on top
// (its session/account tables live alongside these; org+membership are shared).

import {
  pgTable,
  text,
  integer,
  boolean,
  timestamp,
  primaryKey,
} from "drizzle-orm/pg-core";

export const orgs = pgTable("orgs", {
  id: text("id").primaryKey(),
  name: text("name").notNull(),
  slug: text("slug").notNull().unique(),
  plan: text("plan").notNull().default("free"), // free | cloud
  createdAt: timestamp("created_at").notNull().defaultNow(),
});

export const users = pgTable("users", {
  id: text("id").primaryKey(),
  name: text("name").notNull(),
  email: text("email").notNull().unique(),
  createdAt: timestamp("created_at").notNull().defaultNow(),
});

export const memberships = pgTable("memberships", {
  id: text("id").primaryKey(),
  orgId: text("org_id")
    .notNull()
    .references(() => orgs.id, { onDelete: "cascade" }),
  userId: text("user_id")
    .notNull()
    .references(() => users.id, { onDelete: "cascade" }),
  role: text("role").notNull().default("Developer"), // Org Admin | Project Admin | Developer
  createdAt: timestamp("created_at").notNull().defaultNow(),
});

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
  ip: text("ip").notNull().default(""),
  cpu: integer("cpu").notNull().default(0),
  memGb: integer("mem_gb").notNull().default(0),
  byoVpn: boolean("byo_vpn").notNull().default(false),
  connectedAt: timestamp("connected_at").notNull().defaultNow(),
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
  kind: text("kind").notNull(), // app | postgres | mysql | mongo | redis | s3 | llm
  status: text("status").notNull().default("provisioning"),
  repo: text("repo"),
  domain: text("domain"),
  version: text("version"),
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
