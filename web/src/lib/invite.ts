// P2-7b invite logic — pure, dependency-light helpers kept out of the server
// action so they can be unit-tested like lib/rbac.ts. Token *generation* uses
// node:crypto (not pure), but hashing, expiry, role/grant validation, and URL
// building are all deterministic and covered by lib/invite.test.ts.

import { createHash, randomBytes } from "node:crypto";

/** Invites live for 7 days. Short enough that a leaked link ages out, long
 *  enough that a teammate can accept over a weekend. */
export const INVITE_TTL_MS = 7 * 24 * 60 * 60 * 1000;

export const ORG_ROLES = ["Org Admin", "Project Admin", "Developer"] as const;
export const PROJECT_GRANT_ROLES = ["Project Admin", "Developer"] as const;

export type InviteProjectGrant = { projectId: string; role: string };

/** SHA-256 hex of the raw token. Only the hash is stored; the raw token lives
 *  solely in the emailed link, so a database read can't reconstruct a usable
 *  invite URL. Deterministic → testable. */
export function hashInviteToken(rawToken: string): string {
  return createHash("sha256").update(rawToken).digest("hex");
}

/** A fresh invite token: a URL-safe 32-byte secret plus its stored hash. */
export function newInviteToken(): { raw: string; hash: string } {
  const raw = randomBytes(32).toString("base64url");
  return { raw, hash: hashInviteToken(raw) };
}

/** An invite is usable only while pending and unexpired. */
export function inviteUsable(
  invite: { status: string; expiresAt: Date },
  now: Date
): boolean {
  return invite.status === "pending" && invite.expiresAt.getTime() > now.getTime();
}

/** Why an otherwise-found invite can't be accepted — for honest UI copy. */
export function inviteRejection(
  invite: { status: string; expiresAt: Date } | null,
  now: Date
): "not-found" | "revoked" | "accepted" | "expired" | null {
  if (!invite) return "not-found";
  if (invite.status === "revoked") return "revoked";
  if (invite.status === "accepted") return "accepted";
  if (invite.expiresAt.getTime() <= now.getTime()) return "expired";
  return null;
}

/** Normalize an org role, defaulting unknown values to the least-privileged. */
export function normalizeOrgRole(role: string): string {
  return (ORG_ROLES as readonly string[]).includes(role) ? role : "Developer";
}

/** Human-readable reason an invite can't be accepted, for page/toast copy. */
export function inviteRejectionMessage(
  r: "not-found" | "revoked" | "accepted" | "expired"
): string {
  switch (r) {
    case "not-found":
      return "This invitation link is invalid.";
    case "revoked":
      return "This invitation has been revoked.";
    case "accepted":
      return "This invitation has already been accepted.";
    case "expired":
      return "This invitation has expired. Ask an admin to resend it.";
  }
}

/** Parse + validate the stored project-grant JSON. Anything malformed collapses
 *  to an empty list rather than throwing — a corrupt grants blob must never
 *  block accepting the org membership. */
export function parseProjectGrants(json: string): InviteProjectGrant[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(json);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const out: InviteProjectGrant[] = [];
  for (const g of parsed) {
    if (
      g &&
      typeof g === "object" &&
      typeof (g as InviteProjectGrant).projectId === "string" &&
      (PROJECT_GRANT_ROLES as readonly string[]).includes((g as InviteProjectGrant).role)
    ) {
      out.push({ projectId: (g as InviteProjectGrant).projectId, role: (g as InviteProjectGrant).role });
    }
  }
  return out;
}

/** Serialize grants for storage, dropping any invalid entries first. */
export function serializeProjectGrants(grants: InviteProjectGrant[]): string {
  const clean = grants.filter(
    (g) => typeof g.projectId === "string" && (PROJECT_GRANT_ROLES as readonly string[]).includes(g.role)
  );
  return JSON.stringify(clean);
}

/** Build the accept URL from the app's public base and the raw token. */
export function inviteUrl(baseUrl: string, rawToken: string): string {
  return `${baseUrl.replace(/\/+$/, "")}/invite/${encodeURIComponent(rawToken)}`;
}

/** The app's public origin for links (mirrors better-auth's BETTER_AUTH_URL). */
export function appBaseUrl(): string {
  return (
    process.env.BETTER_AUTH_URL ||
    process.env.WEB_PUBLIC_URL ||
    "http://localhost:3000"
  );
}

/** Two emails address the same account iff they match case-insensitively. */
export function sameEmail(a: string, b: string): boolean {
  return a.trim().toLowerCase() === b.trim().toLowerCase();
}
