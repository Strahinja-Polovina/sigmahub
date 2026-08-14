// Does this deployment actually deliver email? (SIGMA-307)
//
// No SMTP client is bundled with SigmaHub. Every outbound message the product
// has — the invite link, the email-verification link, the password-reset link —
// is written to the web container's stdout by its sender and goes nowhere else.
// The invite flow was honest about that from the start: sendInviteEmail returns
// { delivered: false } and the dialog offers the link to copy. The reset flow
// was not: it told the user to check their inbox, and then their spam folder,
// for mail that was never sent — stranding the one user who cannot ask anyone
// to fix it from inside the product.
//
// So the answer lives in ONE function, and both the sender and the screens that
// describe what the sender did read it. Wiring a real transport means adding its
// branch here in the same commit that adds the transport — which is the only
// arrangement in which the copy and the behaviour cannot drift apart.
//
// Deliberately not a NEXT_PUBLIC_ variable, for the reason lib/auth-providers.ts
// spells out: those are inlined at build time and SigmaHub is self-hosted from a
// prebuilt image, so an operator wiring SMTP on Tuesday must not have to rebuild
// the web bundle for the UI to stop apologising.

/** The transports this build knows how to use. `log` means "printed where the
 *  operator can find it, delivered to nobody"; `smtp` is a real submission
 *  server and is what makes mailDelivers() answer true (SIGMA-365).
 *
 *  The descriptor carries NO credentials on purpose. This module is plain lib/
 *  code that a client component could import, and while Next never inlines a
 *  non-NEXT_PUBLIC_ variable into a client bundle, the password simply has no
 *  business in a value the UI reads — server/smtp.ts reads it directly instead. */
export type MailTransport =
  | { kind: "log" }
  | { kind: "smtp"; host: string; port: number; from: string };

/** Submission port when SMTP_PORT is unset. 587 (STARTTLS), not 25. */
export const defaultSmtpPort = 587;

/**
 * The addr-spec inside an address, for the SMTP envelope (SIGMA-365).
 *
 * MAIL FROM / RCPT TO take a bare `<user@host>` — a display name belongs in the
 * `From:`/`To:` HEADER and nowhere else. Interpolating the configured value
 * verbatim put `MAIL FROM:<SigmaHub <no-reply@example.com>>` on the wire, which
 * every RFC-conforming server rejects with `501 5.5.4 Syntax`, and the same for
 * an operator who wrote the address already wrapped in angle brackets. Both are
 * the natural things to write, and the failure was silent all the way to the
 * user, so the two forms are separated here rather than trusted to configuration.
 *
 * Lives in lib/ rather than next to the SMTP client because the boot check below
 * has to apply the same rule the wire does — a value that passes validation and
 * then gets parsed differently at send time is the bug this is meant to close.
 */
export function envelopeAddress(address: string): string {
  // CR/LF first: a crafted value must not be able to smuggle in a header, and
  // the angle-bracket match must not straddle a line break either.
  const v = address.replace(/[\r\n]+/g, " ").trim();
  const angled = /<([^<>]+)>\s*$/.exec(v);
  return (angled ? angled[1] : v).trim();
}

/**
 * Is this a submittable envelope sender? Deliberately not an RFC 5321 parser —
 * it only has to reject the values that make a real server hang up, which in
 * practice means "no @ in it at all" and "nothing on one side of the @".
 */
function isAddrSpec(v: string): boolean {
  return /^[^\s@]+@[^\s@]+$/.test(v);
}

export function configuredMailTransport(): MailTransport {
  const host = (process.env.SMTP_HOST ?? "").trim();
  const from = (process.env.SMTP_FROM ?? "").trim();
  // Both, or neither: a host with no envelope sender cannot submit, and an
  // operator who set one of the two has a half-configured deployment that should
  // keep saying "not delivered" rather than start claiming an inbox.
  if (!host || !from) return { kind: "log" };
  // A malformed sender fails HERE, at the first import of this module, and not at
  // the first password reset (SIGMA-365). Every consumer of this transport is a
  // flow nobody watches: the reset mail is sent for a user who is already locked
  // out, the verification mail during a sign-up nobody is tailing logs for. A
  // `501 5.5.4 Syntax` on those reaches the operator as "some users say they get
  // no mail", weeks later. Refusing the value at boot is the same contract
  // parseBoolEnv gives AUTH_REQUIRE_EMAIL_VERIFICATION in lib/auth.ts, and both
  // are read at that module's top level, so the deployment does not start.
  if (!isAddrSpec(envelopeAddress(from))) {
    throw new Error(
      `SMTP_FROM must be an email address (got ${JSON.stringify(from)}). ` +
        `Write a bare address such as no-reply@example.com, or a display-name ` +
        `form such as "SigmaHub <no-reply@example.com>".`
    );
  }
  const port = Number.parseInt((process.env.SMTP_PORT ?? "").trim(), 10);
  return {
    kind: "smtp",
    host,
    from,
    port: Number.isFinite(port) && port > 0 ? port : defaultSmtpPort,
  };
}

/** Whether a message handed to a sender reaches the recipient's mailbox. False
 *  on every deployment today — the screens say so rather than guessing. */
export function mailDelivers(
  transport: MailTransport = configuredMailTransport()
): boolean {
  return transport.kind !== "log";
}
