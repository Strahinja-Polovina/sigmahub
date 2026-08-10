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

/** The transports this build knows how to use. `log` — the only one today —
 *  means "printed where the operator can find it, delivered to nobody". */
export type MailTransport = { kind: "log" };

export function configuredMailTransport(): MailTransport {
  // Nothing to configure yet: adding an SMTP/API sender means reading its
  // settings out of the environment here and returning its descriptor, at
  // which point mailDelivers() below starts answering true on the deployments
  // that set them.
  return { kind: "log" };
}

/** Whether a message handed to a sender reaches the recipient's mailbox. False
 *  on every deployment today — the screens say so rather than guessing. */
export function mailDelivers(
  transport: MailTransport = configuredMailTransport()
): boolean {
  return transport.kind !== "log";
}
