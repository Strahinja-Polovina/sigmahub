import "server-only";

// P2-7b invite delivery. No SMTP is bundled — same honest stance as
// better-auth's sendResetPassword (lib/auth.ts): in dev / self-hosted the link
// goes to the server log so the operator can relay it, and the invite action
// ALSO returns the URL so an admin can copy it straight from the dialog. Wire a
// real transport here for hosted deployments (the CP's alert channels already
// prove out SMTP/webhook senders server-side).
export async function sendInviteEmail(opts: {
  to: string;
  orgName: string;
  role: string;
  url: string;
}): Promise<{ delivered: boolean }> {
  console.info(
    `[invite] ${opts.to} invited to ${opts.orgName} as ${opts.role}: ${opts.url}`
  );
  // No transport wired → not actually delivered. The caller surfaces the link
  // so an admin can relay it by hand; a hosted deployment returns true here.
  return { delivered: false };
}
