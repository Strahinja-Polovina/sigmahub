"use server";

// SIGMA-284 — erasure and export for customer personal data.
//
// ── THE PII INVENTORY ───────────────────────────────────────────────────────
//
// Written down here, next to the code that acts on it, because the reason
// nobody could answer a GDPR Art. 17 request was not that DELETE is hard. It
// was that the personal data is spread over TWO databases in two schemas
// (Drizzle TypeScript here, Go migrations in cp/internal/store/migrations) with
// no map, and an operator hand-writing DELETEs will miss the session table and
// both audit logs every time.
//
// WEB DATABASE (this schema)
//   user                    name, email (unique), image, verification flag
//   session                 IP ADDRESS, user agent, session token   → FK cascade
//   account                 password hash, OAuth access/refresh/id tokens → FK cascade
//   two_factor              TOTP secret, backup codes               → FK cascade
//   verification            identifier = the EMAIL (reset/verify flows) — NO FK,
//                           so nothing cascades it; deleted explicitly below
//   memberships             user_id                                 → FK cascade
//   project_memberships     user_id                                 → FK cascade
//   invitations             email of the invitee, invited_by = an actor's
//                           DISPLAY NAME — no FK to user; deleted/redacted here
//   audit_log               actor and target are DISPLAY NAMES, org_id is a bare
//                           text column with no FK, so an org delete leaves it
//                           behind entirely
//   deployments.author      a git commit author. Provenance of a build, not an
//                           account: it survives an account deletion the way the
//                           git history it was copied from does. Called out so
//                           the omission is a decision and not an oversight.
//
// CONTROL-PLANE DATABASE (cp/internal/store/migrations)
//   cp_audit_log            actor display name on every row
//   alert_channels          SMTP credentials and the recipient addresses paged
//   dns_provider_credentials  API tokens for the customer's own DNS account
//   git_connections, org_registries, db_credentials, s3_credentials, secrets,
//   org_deks                the tenant's credentials and key material
//   …and ~30 more org-scoped tables. Rather than restate that list in a second
//   place that will rot, the CP discovers it from its own schema: see
//   store.PurgeOrg, reached from here through DELETE /v1/orgs/{orgId}.
//
// ── RETENTION ───────────────────────────────────────────────────────────────
//
// cp_audit_log is already bounded: the sweeper prunes it at 400 days
// (SIGMA-249, cp/internal/store/retention.go + the Retain block in
// cp/cmd/sigmahub-cp/main.go). THIS schema's audit_log is not, and there is no
// scheduler on the web side to prune it from; what bounds it today is
// deleteOrganization, which takes the org's audit rows with it.

import { revalidatePath } from "next/cache";
import { eq, inArray, sql } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import * as auth from "../db/auth-schema";
import { getSessionUser, requireOrgAdmin } from "../active-org";
import { writeAudit } from "../audit";
import { cpEnabled, cpPurgeOrg } from "../cp";

/** What an audit row's actor/target becomes once the person behind it is gone.
 *  The ROW stays: "who changed this, and when" is the org's record and the
 *  other members' data too. Only the identifier is removed. */
const REDACTED = "Deleted user";

/** Personal data as handed back by the export actions. Deliberately plain
 *  JSON — the point is that a partner can read it without our code. */
export type PersonalDataExport = {
  exportedAt: string;
  /** What this export deliberately does NOT contain, so the reader is not left
   *  guessing whether an empty section means "none" or "withheld". */
  excluded: string[];
  [section: string]: unknown;
};

/** Replace every occurrence of a person's identifiers in the audit trail of the
 *  orgs they belonged to. Substring rather than equality because targets read
 *  like "Dana Reeve → Org Admin" — an equality match would leave those intact
 *  and report success. */
async function redactActorStrings(orgIds: string[], identifiers: string[]) {
  const needles = identifiers.map((v) => v.trim()).filter((v) => v.length >= 3);
  if (orgIds.length === 0 || needles.length === 0) return;
  for (const needle of needles) {
    await db
      .update(s.auditLog)
      .set({
        actor: sql`replace(${s.auditLog.actor}, ${needle}, ${REDACTED})`,
        target: sql`replace(${s.auditLog.target}, ${needle}, ${REDACTED})`,
      })
      .where(inArray(s.auditLog.orgId, orgIds));
    await db
      .update(s.invitations)
      .set({ invitedBy: sql`replace(${s.invitations.invitedBy}, ${needle}, ${REDACTED})` })
      .where(inArray(s.invitations.orgId, orgIds));
  }
}

/** Everything the web database holds about one person, for Art. 15/20. */
export async function exportUserData(): Promise<PersonalDataExport> {
  const me = await getSessionUser();
  const [account] = await db
    .select()
    .from(auth.user)
    .where(eq(auth.user.id, me.id));
  const sessions = await db
    .select({
      createdAt: auth.session.createdAt,
      expiresAt: auth.session.expiresAt,
      ipAddress: auth.session.ipAddress,
      userAgent: auth.session.userAgent,
    })
    .from(auth.session)
    .where(eq(auth.session.userId, me.id));
  const logins = await db
    .select({
      providerId: auth.account.providerId,
      accountId: auth.account.accountId,
      createdAt: auth.account.createdAt,
    })
    .from(auth.account)
    .where(eq(auth.account.userId, me.id));
  const orgs = await db
    .select({
      orgId: s.memberships.orgId,
      orgName: s.orgs.name,
      role: s.memberships.role,
      scoped: s.memberships.scoped,
      joinedAt: s.memberships.createdAt,
    })
    .from(s.memberships)
    .innerJoin(s.orgs, eq(s.orgs.id, s.memberships.orgId))
    .where(eq(s.memberships.userId, me.id));
  const projectGrants = await db
    .select({
      projectId: s.projectMemberships.projectId,
      role: s.projectMemberships.role,
      grantedAt: s.projectMemberships.createdAt,
    })
    .from(s.projectMemberships)
    .where(eq(s.projectMemberships.userId, me.id));

  return {
    exportedAt: new Date().toISOString(),
    // Credential material is not exported. It is the one category where handing
    // a copy to whoever holds the session is a bigger risk to the person than
    // withholding it, and none of it is data they gave us to read back.
    excluded: [
      "password hash",
      "OAuth access/refresh tokens",
      "two-factor secret and backup codes",
      "session tokens",
    ],
    account: account
      ? {
          id: account.id,
          name: account.name,
          email: account.email,
          emailVerified: account.emailVerified,
          image: account.image,
          twoFactorEnabled: account.twoFactorEnabled,
          createdAt: account.createdAt,
        }
      : null,
    sessions,
    logins,
    organizations: orgs,
    projectGrants,
  };
}

/** Everything the web database holds about one organization, for a partner who
 *  asks for their data back before it is deleted. Org Admin only.
 *
 *  Web-side only, on purpose: the control plane's copy is reachable through the
 *  existing per-object read endpoints, and duplicating them into a second
 *  serializer is how the two drift. What is NOT reachable any other way — the
 *  membership list, the invite list and the audit trail — is what this is for.
 */
export async function exportOrganization(input: {
  orgId: string;
}): Promise<PersonalDataExport> {
  await requireOrgAdmin(input.orgId);
  const [org] = await db.select().from(s.orgs).where(eq(s.orgs.id, input.orgId));
  if (!org) throw new Error("Organization not found.");

  const members = await db
    .select({
      userId: s.memberships.userId,
      name: auth.user.name,
      email: auth.user.email,
      role: s.memberships.role,
      scoped: s.memberships.scoped,
      joinedAt: s.memberships.createdAt,
    })
    .from(s.memberships)
    .innerJoin(auth.user, eq(auth.user.id, s.memberships.userId))
    .where(eq(s.memberships.orgId, input.orgId));
  const invites = await db
    .select({
      email: s.invitations.email,
      role: s.invitations.role,
      status: s.invitations.status,
      invitedBy: s.invitations.invitedBy,
      createdAt: s.invitations.createdAt,
      acceptedAt: s.invitations.acceptedAt,
    })
    .from(s.invitations)
    .where(eq(s.invitations.orgId, input.orgId));
  const projects = await db
    .select()
    .from(s.projects)
    .where(eq(s.projects.orgId, input.orgId));
  const servers = await db
    .select()
    .from(s.servers)
    .where(eq(s.servers.orgId, input.orgId));
  // The whole audit log, not the 50 rows the dashboard paginates: an export
  // that silently truncates is not an export.
  const audit = await db
    .select()
    .from(s.auditLog)
    .where(eq(s.auditLog.orgId, input.orgId));

  await writeAudit({
    orgId: input.orgId,
    actor: (await getSessionUser()).name,
    action: "Exported organization data",
    target: org.name,
  });

  return {
    exportedAt: new Date().toISOString(),
    excluded: [
      "secret values (held encrypted by the control plane)",
      "session tokens and credential material",
    ],
    organization: org,
    members,
    invitations: invites,
    projects,
    servers,
    auditLog: audit,
  };
}

/** Delete an organization: its control-plane tenant first, then every web row.
 *
 *  CP first on purpose. If the CP purge fails we stop with the web rows intact,
 *  which is recoverable — the operator retries. The other order would leave the
 *  control plane holding a tenant the dashboard can no longer name, address, or
 *  even list, which is not.
 */
export async function deleteOrganization(input: {
  orgId: string;
  /** The org's name, retyped. A delete that takes a fleet's records with it
   *  should cost more than one click. */
  confirmName: string;
}): Promise<void> {
  await requireOrgAdmin(input.orgId);
  const [org] = await db.select().from(s.orgs).where(eq(s.orgs.id, input.orgId));
  if (!org) throw new Error("Organization not found.");
  if (input.confirmName.trim() !== org.name) {
    throw new Error("Type the organization's name exactly to confirm deletion.");
  }
  // Not audited: the only log this org has is the one being deleted, and
  // writing the actor's name into a table we are about to empty — for erasure —
  // would be the opposite of the point. The control plane logs its side at warn
  // (handlePurgeOrg), which is where an operator looks for this.
  await purgeOrgRows(input.orgId);
  revalidatePath("/dashboard", "layout");
}

/** The org teardown itself, shared with deleteUser (which takes the orgs that
 *  would be left with no members at all). No permission check — both callers
 *  have already made one, and this must not become a second, weaker gate. */
async function purgeOrgRows(orgId: string): Promise<void> {
  if (cpEnabled()) await cpPurgeOrg(orgId);
  // audit_log.org_id is a bare text column with no foreign key, so it is the
  // one table the org cascade does not reach — and the one holding names.
  await db.delete(s.auditLog).where(eq(s.auditLog.orgId, orgId));
  // Everything else (memberships, invitations, projects → environments →
  // resources, servers, clusters) hangs off orgs with ON DELETE CASCADE.
  await db.delete(s.orgs).where(eq(s.orgs.id, orgId));
}

/** Erase the signed-in person: Art. 17.
 *
 *  Self-service by design. An admin can already remove somebody from an org
 *  (removeMember); erasing the ACCOUNT is a different act, and letting one
 *  member delete another's identity — including the orgs they own elsewhere —
 *  is not a power an org role should carry.
 *
 *  Orgs are handled before the account, because the account is what proves
 *  which orgs are involved:
 *    • an org where this was the only member is deleted with them (CP tenant
 *      included) — leaving it behind means an unreachable org holding a fleet;
 *    • an org where this was the ONLY admin but others remain is refused,
 *      naming the org. Deleting anyway would lock every remaining member out
 *      permanently, which is somebody else's data lost to somebody else's
 *      erasure request.
 */
export async function deleteUser(input: {
  /** The account's own email, retyped. */
  confirmEmail: string;
}): Promise<void> {
  const me = await getSessionUser();
  const [account] = await db.select().from(auth.user).where(eq(auth.user.id, me.id));
  if (!account) throw new Error("Account not found.");
  if (input.confirmEmail.trim().toLowerCase() !== account.email.toLowerCase()) {
    throw new Error("Type your email address exactly to confirm deletion.");
  }

  const mine = await db
    .select({ orgId: s.memberships.orgId, role: s.memberships.role })
    .from(s.memberships)
    .where(eq(s.memberships.userId, account.id));
  const orgIds = mine.map((m) => m.orgId);

  const doomed: string[] = [];
  for (const m of mine) {
    const others = await db
      .select({ userId: s.memberships.userId, role: s.memberships.role })
      .from(s.memberships)
      .where(eq(s.memberships.orgId, m.orgId));
    if (others.length === 1) {
      doomed.push(m.orgId);
      continue;
    }
    const admins = others.filter((o) => o.role === "Org Admin");
    if (admins.length === 1 && admins[0].userId === account.id) {
      const [org] = await db.select().from(s.orgs).where(eq(s.orgs.id, m.orgId));
      throw new Error(
        `You are the only admin of ${org?.name ?? m.orgId}. Promote another admin, or delete the organization first.`
      );
    }
  }

  // Redact BEFORE the org teardown: a doomed org's rows are about to go anyway,
  // and a surviving org's audit trail must lose the name while it still exists.
  await redactActorStrings(orgIds, [account.name, account.email]);
  for (const orgId of doomed) await purgeOrgRows(orgId);

  // The account itself. session, account, two_factor, memberships and
  // project_memberships all reference user.id ON DELETE CASCADE.
  await db.delete(auth.user).where(eq(auth.user.id, account.id));
  // verification identifies by EMAIL and has no foreign key, so nothing above
  // touches it: a password-reset row would otherwise outlive the account it
  // belongs to, holding the address in plain text.
  await db
    .delete(auth.verification)
    .where(sql`lower(${auth.verification.identifier}) = ${account.email.toLowerCase()}`);
  // Invitations are addressed to an email, not to a user id — including ones in
  // orgs this person never joined, which is exactly the kind of row a
  // membership-shaped delete misses.
  await db
    .delete(s.invitations)
    .where(sql`lower(${s.invitations.email}) = ${account.email.toLowerCase()}`);

  revalidatePath("/dashboard", "layout");
}
