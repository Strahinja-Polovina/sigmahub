import "server-only";

import { configuredMailTransport, mailDelivers } from "@/lib/mail";
import { sendSmtpMail } from "./smtp";

// Outbound mail. One door: every message the product sends — invite, email
// verification, password reset — goes through sendMail, so "does this deployment
// deliver mail" has exactly one answer (lib/mail) and the copy on the screens
// cannot drift from what the sender actually did (SIGMA-307).
//
// With SMTP_HOST + SMTP_FROM set, mail is really submitted (SIGMA-365). Without
// them the message is written to the container log, which is genuinely usable
// self-hosted — the operator relays the link — and every screen says so instead
// of pointing the user at a spam folder that will never fill.

/** Credentials live here, not in the lib/ descriptor the UI reads. */
function smtpCredentials() {
  const username = (process.env.SMTP_USERNAME ?? "").trim();
  const password = process.env.SMTP_PASSWORD ?? "";
  // Opt-out for a submission server on a trusted network that offers no
  // STARTTLS. Off by default: these messages carry bearer links.
  const allowInsecure = ["1", "t", "T", "TRUE", "true", "True"].includes(
    (process.env.SMTP_ALLOW_INSECURE ?? "").trim()
  );
  return {
    username: username || undefined,
    password: password || undefined,
    requireTls: !allowInsecure,
  };
}

/**
 * Deliver one plain-text message. Never throws: a send failure is an operator
 * problem, and the callers (invite dialog, better-auth flows) each have an
 * honest fallback that depends on knowing delivery failed rather than on an
 * exception. Returns whether the recipient's mailbox actually received it.
 */
export async function sendMail(opts: {
  to: string;
  subject: string;
  text: string;
}): Promise<{ delivered: boolean }> {
  const transport = configuredMailTransport();
  if (transport.kind !== "smtp") {
    console.info(`[mail] ${opts.to}: ${opts.subject}\n${opts.text}`);
    return { delivered: false };
  }
  const { username, password, requireTls } = smtpCredentials();
  try {
    await sendSmtpMail(
      {
        host: transport.host,
        port: transport.port,
        username,
        password,
        implicitTls: transport.port === 465,
        requireTls,
      },
      {
        from: transport.from,
        to: [opts.to],
        subject: opts.subject,
        text: opts.text,
      }
    );
    return { delivered: true };
  } catch (err) {
    // The operator's signal. Deliberately loud and deliberately not rethrown:
    // the password-reset flow must stay non-enumerating, and the invite dialog
    // falls back to offering the link.
    console.error(
      `[mail] SMTP delivery to ${opts.to} failed via ${transport.host}:${transport.port}:`,
      err instanceof Error ? err.message : err
    );
    return { delivered: false };
  }
}

export async function sendInviteEmail(opts: {
  to: string;
  orgName: string;
  role: string;
  url: string;
}): Promise<{ delivered: boolean }> {
  if (!mailDelivers()) {
    console.info(
      `[invite] ${opts.to} invited to ${opts.orgName} as ${opts.role}: ${opts.url}`
    );
    return { delivered: false };
  }
  return sendMail({
    to: opts.to,
    subject: `You have been invited to ${opts.orgName} on SigmaHub`,
    text:
      `You have been invited to join ${opts.orgName} on SigmaHub as ${opts.role}.\n\n` +
      `Accept the invitation:\n${opts.url}\n\n` +
      `If you were not expecting this, you can ignore this message.\n`,
  });
}
