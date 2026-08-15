"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import { Check, Loader2, LogOut } from "lucide-react";

import { Button } from "@/components/ui/button";
import { acceptInvite } from "@/server/actions/members";
import { authClient } from "@/lib/auth-client";
import { unwrap } from "@/lib/action-result";

/** The interactive tail of the accept page. `matches` is whether the signed-in
 *  account's email equals the invite's — only then can it be accepted; a
 *  mismatch offers to sign out and switch, carrying the token along. */
export function AcceptInvite({
  token,
  matches,
  inviteEmail,
}: {
  token: string;
  matches: boolean;
  inviteEmail: string;
}) {
  const router = useRouter();
  const [pending, startTransition] = React.useTransition();

  if (!matches) {
    return (
      <div className="flex flex-col gap-3">
        <p className="text-sm text-muted-foreground">
          You’re signed in with a different account. This invitation is for{" "}
          <span className="font-medium text-foreground">{inviteEmail}</span>.
        </p>
        <Button
          variant="outline"
          onClick={() =>
            authClient
              .signOut()
              .finally(() => router.push(`/login?invite=${encodeURIComponent(token)}`))
          }
        >
          <LogOut className="size-4" />
          Sign out &amp; switch account
        </Button>
      </div>
    );
  }

  function accept() {
    startTransition(async () => {
      try {
        const { orgId } = unwrap(await acceptInvite({ token }));
        // Make the just-joined org the active one so the dashboard opens on it.
        document.cookie = `sh_org=${orgId}; path=/; max-age=31536000; samesite=lax`;
        toast.success("Invitation accepted");
        router.push("/dashboard");
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t accept invitation", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Button onClick={accept} disabled={pending} className="w-full">
      {pending ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
      Accept invitation
    </Button>
  );
}
