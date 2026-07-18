import Link from "next/link";
import { headers } from "next/headers";
import { UserPlus } from "lucide-react";

import { auth } from "@/lib/auth";
import { getInviteByToken } from "@/server/queries";
import { inviteRejection, inviteRejectionMessage, sameEmail } from "@/lib/invite";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { AcceptInvite } from "./accept-invite";

// P2-7b invite accept page. Deliberately OUTSIDE the (auth) route group — that
// layout redirects any signed-in user to /dashboard, which would break
// accepting while logged in. Read-only status/expiry judgement here; the actual
// membership + grant materialization happens in the acceptInvite server action.
export default async function InvitePage({
  params,
}: {
  params: Promise<{ token: string }>;
}) {
  const { token } = await params;
  const invite = await getInviteByToken(token);
  const now = new Date();
  const rejection = inviteRejection(
    invite ? { status: invite.status, expiresAt: invite.expiresAt } : null,
    now
  );
  const session = await auth.api.getSession({ headers: await headers() });
  const encoded = encodeURIComponent(token);

  return (
    <main className="grid min-h-screen place-items-center bg-muted/30 p-4">
      <div className="w-full max-w-md rounded-2xl border border-border bg-card p-6 shadow-sm">
        <div className="mb-5 flex flex-col items-center gap-2 text-center">
          <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
            <UserPlus className="size-5" />
          </div>
          <h1 className="text-lg font-semibold tracking-tight text-foreground">
            Team invitation
          </h1>
        </div>

        {rejection || !invite ? (
          <div className="flex flex-col items-center gap-4 text-center">
            <p className="text-sm text-muted-foreground">
              {inviteRejectionMessage(rejection ?? "not-found")}
            </p>
            <Button variant="outline" render={<Link href="/login" />}>
              Go to SigmaHub
            </Button>
          </div>
        ) : (
          <div className="flex flex-col gap-5">
            <div className="rounded-lg border border-border bg-muted/40 p-4 text-center">
              <p className="text-sm text-foreground">
                You’ve been invited to join{" "}
                <span className="font-semibold">{invite.orgName}</span>
              </p>
              <div className="mt-2 flex items-center justify-center gap-2">
                <Badge variant="secondary">{invite.role}</Badge>
                <span className="font-mono text-xs text-muted-foreground">{invite.email}</span>
              </div>
            </div>

            {!session ? (
              <div className="flex flex-col gap-3">
                <p className="text-center text-sm text-muted-foreground">
                  Sign in or create an account with{" "}
                  <span className="font-medium text-foreground">{invite.email}</span> to
                  accept.
                </p>
                <Button className="w-full" render={<Link href={`/signup?invite=${encoded}`} />}>
                  Create account
                </Button>
                <Button
                  variant="outline"
                  className="w-full"
                  render={<Link href={`/login?invite=${encoded}`} />}
                >
                  I already have an account
                </Button>
              </div>
            ) : (
              <div className="flex flex-col gap-3">
                <p className="text-center text-xs text-muted-foreground">
                  Signed in as{" "}
                  <span className="font-medium text-foreground">{session.user.email}</span>
                </p>
                <AcceptInvite
                  token={token}
                  matches={sameEmail(session.user.email, invite.email)}
                  inviteEmail={invite.email}
                />
              </div>
            )}
          </div>
        )}
      </div>
    </main>
  );
}
