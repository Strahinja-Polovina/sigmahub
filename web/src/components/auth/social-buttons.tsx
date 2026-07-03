"use client";

import { KeyRound } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { GitHubMark, GoogleMark } from "@/components/auth/brand-icons";

// SSO / passkey buttons are visual-only in the prototype: clicking them just
// surfaces a toast so the flow feels alive without any real auth wiring.

export function SocialButtons({ action = "continue" }: { action?: string }) {
  const notify = (provider: string) =>
    toast.info(`${provider} sign-in`, {
      description: "Single sign-on is not wired up in this prototype.",
    });

  return (
    <div className="grid gap-2">
      <Button
        type="button"
        variant="outline"
        className="w-full justify-center"
        onClick={() => notify("Google")}
      >
        <GoogleMark className="size-4" />
        {`${cap(action)} with Google`}
      </Button>
      <Button
        type="button"
        variant="outline"
        className="w-full justify-center"
        onClick={() => notify("GitHub")}
      >
        <GitHubMark className="size-4" />
        {`${cap(action)} with GitHub`}
      </Button>
      <Button
        type="button"
        variant="outline"
        className="w-full justify-center"
        onClick={() => notify("Passkey")}
      >
        <KeyRound className="size-4" />
        {`${cap(action)} with a passkey`}
      </Button>
    </div>
  );
}

function cap(s: string) {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
