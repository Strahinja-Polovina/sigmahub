"use client";

import * as React from "react";
import { toast } from "sonner";

import { authClient } from "@/lib/auth-client";
import { destAfterAuth } from "@/lib/after-auth";
import { Button } from "@/components/ui/button";
import { GitHubMark, GoogleMark } from "@/components/auth/brand-icons";
import { useAuthProviders } from "@/components/auth/auth-providers";

// Third-party sign-in, shown only for providers this deployment configured
// (SIGMA-246).
//
// These used to be decoration: all three rendered on every deployment and each
// one toasted "Single sign-on is not wired up in this prototype." — including
// the GitHub button, which is the first thing a developer evaluating a
// deploy-from-GitHub product clicks, on the screen before they have an account.
//
// Now the flags come from the server (see lib/auth-providers.ts) and the same
// credentials configure better-auth's socialProviders, so a button exists only
// when the flow behind it does. The passkey button is gone rather than gated:
// better-auth is set up here without the passkey plugin, so there is no
// credential to configure and nothing the button could ever start. Re-add it
// with the plugin.

export function SocialButtons({ action = "continue" }: { action?: string }) {
  const providers = useAuthProviders();
  const [pending, setPending] = React.useState<string | null>(null);

  async function go(provider: "google" | "github", label: string) {
    setPending(provider);
    const { error } = await authClient.signIn.social({
      provider,
      callbackURL: destAfterAuth(),
    });
    // On success the browser is already navigating to the provider; only the
    // failure path comes back here.
    setPending(null);
    if (error) {
      toast.error(`Couldn’t continue with ${label}`, {
        description: error.message ?? "Please try again.",
      });
    }
  }

  return (
    <div className="grid gap-2">
      {providers.google && (
        <Button
          type="button"
          variant="outline"
          className="w-full justify-center"
          disabled={pending !== null}
          onClick={() => go("google", "Google")}
        >
          <GoogleMark className="size-4" />
          {`${cap(action)} with Google`}
        </Button>
      )}
      {providers.github && (
        <Button
          type="button"
          variant="outline"
          className="w-full justify-center"
          disabled={pending !== null}
          onClick={() => go("github", "GitHub")}
        >
          <GitHubMark className="size-4" />
          {`${cap(action)} with GitHub`}
        </Button>
      )}
    </div>
  );
}

function cap(s: string) {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
