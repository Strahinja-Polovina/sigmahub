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
    // When this invitation was last MAILED — which created_at is not, because a
    // resend does not create a row. It is what makes the resend cooldown
    // possible (SIGMA-365); without it, holding the resend button mailed an
    // arbitrary address without limit from our sending domain.
    lastSentAt: timestamp("last_sent_at").notNull().defaultNow(),
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

// Outbound-mail budget, one row per organization (SIGMA-365).
//
// Sign-up makes every new account an Org Admin of its own org, so on a public
// launch the invite flow is reachable by anyone who registered — and it mails
// arbitrary addresses from the sending domain that every other tenant's
// password-reset mail depends on. A blocklisted domain is not fixed by
// deploying a patch.
//
// This exists because the obvious bounds do not bound anything. A per-invitation
// resend cooldown limits how often ONE row is mailed; a cap on invitations
// created per hour limits how many new rows appear. Their product is the real
// limit — 25 pending invites resent once a minute each is 1500 messages an hour.
// Counting sends is the only thing that bounds sends.
//
// window_start is when the window OPENED, not when the last message went out: a
// sliding last-send timestamp would let a steady drip push the window forward
// forever and never reset the count.
export const orgMailBudget = pgTable("org_mail_budget", {
  orgId: text("org_id")
    .primaryKey()
    .references(() => orgs.id, { onDelete: "cascade" }),
  windowStart: timestamp("window_start").notNull().defaultNow(),
  sent: integer("sent").notNull().default(0),
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
  // A server type from the control plane's catalog: SERVER_TYPES in
  // @/lib/server-catalog.generated, rendered from cp/internal/store. The types
  // are NOT restated here. They used to be, and the list stayed four names long
  // through the arrival of the VPS, the cluster node and the build server — the
  // file that defines the column described a fleet three types smaller than the
  // one it stores, and nothing could notice, because a comment is the one copy
  // of a vocabulary that no generator writes and no test used to read. So the
  // only thing written down here is where the vocabulary lives (SIGMA-216).
  type: text("type").notNull(),
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
  // A graceful decommission in flight (SIGMA-204). Set when the operator
  // disconnects, cleared only by the row going away — the tombstone is written
  // when the agent confirms it removed itself, or when the timeout gives up.
  // The dashboard needs the TIMESTAMP and not just the status: "Force
  // disconnect" is offered once the graceful path has had its chance, and that
  // is a comparison against this, not a second flag.
  decommissionStartedAt: timestamp("decommission_started_at"),
  // Whether the operator opted into destroying named volumes too. Default off:
  // a database's data directory is the customer's, and disconnecting the
  // machine it sits on is not consent to delete it.
  decommissionPurgeVolumes: boolean("decommission_purge_volumes").notNull().default(false),
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

// ── Kubernetes clusters, demo side (SIGMA-215) ──────────────────────────────
//
// In CP mode a cluster lives in the control plane and never touches these
// tables; getServers-style mirroring is not enough for it, because a cluster is
// not a server and the dashboard reads a whole node list off it. Demo mode has
// no control plane at all, so the rows ARE the clusters: the seed writes them,
// the panel reads them, and creating one in the dashboard inserts here.
//
// The columns are CpCluster/CpClusterNode field for field. That is deliberate:
// listClusters returns one type in both modes, and a demo shape that only
// mostly matched would put the divergence inside the mapping function rather
// than in the type checker.
export const clusters = pgTable("clusters", {
  id: text("id").primaryKey(),
  orgId: text("org_id")
    .notNull()
    .references(() => orgs.id, { onDelete: "cascade" }),
  // One cluster per environment, so "deploy to the cluster" is unambiguous —
  // the same rule the control plane enforces, and what makes clusterOptions'
  // environment filter meaningful rather than decorative.
  environmentId: text("environment_id")
    .notNull()
    .references(() => environments.id, { onDelete: "cascade" }),
  name: text("name").notNull(),
  status: text("status").notNull().default("provisioning"), // provisioning | ready | degraded
  apiEndpoint: text("api_endpoint").notNull().default(""),
  kubernetesVersion: text("kubernetes_version").notNull().default(""),
  createdBy: text("created_by").notNull().default(""),
  createdAt: timestamp("created_at").notNull().defaultNow(),
});

export const clusterNodes = pgTable(
  "cluster_nodes",
  {
    clusterId: text("cluster_id")
      .notNull()
      .references(() => clusters.id, { onDelete: "cascade" }),
    serverId: text("server_id")
      .notNull()
      .references(() => servers.id, { onDelete: "cascade" }),
    role: text("role").notNull().default("worker"), // control-plane | worker
    // What the node reported about k3s ON it — pending | ready | error. Kept
    // apart from the server's own status for the reason the panel already
    // renders separately: an agent can heartbeat perfectly on a host where
    // Kubernetes never installed, and collapsing the two is how a cluster looks
    // healthy while nothing can be scheduled on it.
    nodeStatus: text("node_status").notNull().default("pending"),
    nodeMessage: text("node_message").notNull().default(""),
    joinedAt: timestamp("joined_at").notNull().defaultNow(),
    reportedAt: timestamp("reported_at"),
  },
  (t) => ({ pk: primaryKey({ columns: [t.clusterId, t.serverId] }) })
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
  // A workload deployed INTO a cluster has no server: the scheduler picks the
  // node. Exactly one of server_id and cluster_id is set, which is what
  // resolveDeployTarget decides and what the control plane enforces.
  //
  // Demo mode had neither column filled for a cluster deploy, so a resource the
  // user targeted at a cluster was written with no target at all and rendered
  // as unassigned — the wizard's cluster choice reached the create call and
  // then evaporated (SIGMA-215).
  clusterId: text("cluster_id").references(() => clusters.id, {
    onDelete: "set null",
  }),
  name: text("name").notNull(),
  // A resource kind from the same catalog: RESOURCE_KINDS in
  // @/lib/server-catalog.generated. Named rather than listed, for the reason
  // the servers.type comment gives — a hand-typed vocabulary in a comment
  // cannot be generated and cannot be checked, so it goes stale in silence.
  //
  // Stored as plain text rather than `.$type<ResourceKind>()` on purpose: this
  // table is a MIRROR of the control plane, and the control plane is what
  // decides which kinds exist. Declaring the column to be a member of the union
  // THIS build was generated against would be a claim about rows this process
  // did not write — a newer control plane's kind would arrive typed as a member
  // of a set it is not in, and every exhaustive read of it would be wrong with
  // no error to show for it. Readers narrow with isResourceKind or print
  // through resourceKindLabel, which is why both of those take a plain string.
  kind: text("kind").notNull(),
  status: text("status").notNull().default("provisioning"),
  repo: text("repo"),
  domain: text("domain"),
  version: text("version"),
  // Which engine serves this resource — the local mirror of the control
  // plane's `spec.engine`, which carries an object-storage engine (minio |
  // seaweedfs) and an inference runtime through the same field.
  //
  // Demo mode had nowhere to put it, so the wizard's engine choice reached
  // createResource and stopped there: every demo S3 resource described itself
  // with the default engine's image and endpoint, and a user who picked
  // SeaweedFS on the storage step opened the resource and was told MinIO
  // (SIGMA-215). Null for the kinds that have no engine to choose — an app —
  // and for a managed database, where the KIND is the engine.
  engine: text("engine"),
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
