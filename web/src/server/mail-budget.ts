import "server-only";

import { sql } from "drizzle-orm";
import { db } from "./db";
import { INVITE_SENDS_PER_WINDOW, INVITE_SEND_WINDOW_MS, humanWait } from "../lib/invite";

// A bound on how much mail one organization can make us send (SIGMA-365).
//
// Sign-up creates a personal org with the new account as Org Admin, so on a
// public launch the invite flow is reachable by anyone who registered. It mails
// arbitrary addresses — the recipient does not have to be a customer — over our
// SMTP, from the sending domain that every other tenant's password-reset and
// verification mail depends on. It is the cheapest abuse in the product to run
// and the most expensive to undo, because a blocklisted domain is not fixed by
// deploying a patch.
//
// It has to count SENDS. The two obvious bounds each look sufficient and are
// not: a per-invitation resend cooldown limits how often one row is mailed, and
// a cap on invitations created per hour limits how many new rows appear — their
// product is the real limit, and 25 pending invites resent once a minute each is
// 1500 messages an hour from one org. That is the version of this that was
// written first, and it did not hold.

/** Thrown when the org has spent its window. Carries the wait so the caller can
 *  say when, rather than just no. */
export class MailBudgetExceeded extends Error {
  constructor(readonly retryAfterMs: number) {
    super(
      `This organization has sent ${INVITE_SENDS_PER_WINDOW} invite emails in the last hour, ` +
        `which is the limit. It resets in ${humanWait(retryAfterMs)} — or copy the invite link ` +
        `from the members list and send it yourself now.`
    );
    this.name = "MailBudgetExceeded";
  }
}

/**
 * Charge one message to `orgId`, or throw MailBudgetExceeded.
 *
 * One statement, so two concurrent sends cannot both read a stale count and both
 * pass — the check-then-increment version of this is exactly the race an
 * attacker supplies concurrency to win.
 *
 * The window rolls forward only when it has fully elapsed. Advancing it on every
 * send instead would let a steady drip hold the window open and never reset the
 * count, which reads as a limit and is not one.
 *
 * Deliberately charges the attempt even when it is refused: the alternative
 * gives a caller who keeps trying a free re-read, and the window still expires
 * on schedule either way.
 */
export async function chargeOrgMail(
  orgId: string,
  limit = INVITE_SENDS_PER_WINDOW,
  windowMs = INVITE_SEND_WINDOW_MS
): Promise<void> {
  const cutoff = new Date(Date.now() - windowMs);
  const rows = await db.execute<{ sent: number; window_start: Date }>(sql`
    INSERT INTO org_mail_budget (org_id, window_start, sent)
         VALUES (${orgId}, now(), 1)
    ON CONFLICT (org_id) DO UPDATE SET
        window_start = CASE WHEN org_mail_budget.window_start < ${cutoff}
                            THEN now() ELSE org_mail_budget.window_start END,
        sent         = CASE WHEN org_mail_budget.window_start < ${cutoff}
                            THEN 1 ELSE org_mail_budget.sent + 1 END
      RETURNING sent, window_start
  `);
  const row = (rows as unknown as { rows?: { sent: number; window_start: Date }[] }).rows?.[0]
    ?? (rows as unknown as { sent: number; window_start: Date }[])[0];
  if (!row) return; // no row back is not a reason to refuse a legitimate invite
  const sent = Number(row.sent);
  if (sent <= limit) return;

  const opened = new Date(row.window_start).getTime();
  throw new MailBudgetExceeded(Math.max(1000, opened + windowMs - Date.now()));
}
