"use client";

import * as React from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { ArrowLeft, CheckCircle2, KeyRound, Loader2, LinkIcon } from "lucide-react";

import { authClient } from "@/lib/auth-client";
import { Button } from "@/components/ui/button";
import { AuthField } from "@/components/auth/auth-field";
import { validateConfirm, validatePassword } from "@/components/auth/validators";

// SIGMA-344. The destination of the reset link, and the only place in the
// product where a password is set from a token rather than from a session.
//
// better-auth mails ${baseURL}/reset-password/<token>?callbackURL=…; opening it
// hits better-auth's OWN GET endpoint, which verifies the token is live and then
// redirects the browser to callbackURL carrying ?token=… (or ?error=INVALID_TOKEN
// when it is expired or already spent). It never sets a password itself — that is
// POST /reset-password, which nothing in this app used to call. /forgot points
// callbackURL here so the token lands on a page that finishes the job.

/** The reset form. Split out because useSearchParams() forces a Suspense
 *  boundary on a statically-rendered route, and the boundary has to sit ABOVE
 *  the component that reads the params. */
function ResetPasswordForm() {
  const params = useSearchParams();
  const token = params.get("token");
  // better-auth's redirect on a dead token. Distinguished from "no token at all"
  // (someone typed the URL) only in wording — the remedy is identical.
  const linkError = params.get("error");

  const [password, setPassword] = React.useState("");
  const [confirm, setConfirm] = React.useState("");
  const [passwordError, setPasswordError] = React.useState<string | null>(null);
  const [confirmError, setConfirmError] = React.useState<string | null>(null);
  const [formError, setFormError] = React.useState<string | null>(null);
  const [submitting, setSubmitting] = React.useState(false);
  const [done, setDone] = React.useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const pErr = validatePassword(password);
    const cErr = validateConfirm(password, confirm);
    setPasswordError(pErr);
    setConfirmError(cErr);
    if (pErr || cErr || !token) return;

    setSubmitting(true);
    setFormError(null);
    try {
      const res = await authClient.resetPassword({ newPassword: password, token });
      if (res.error) throw new Error(res.error.message);
      setDone(true);
    } catch (err) {
      // Unlike /forgot, there is nothing to conceal here: the caller already
      // holds a token that was minted for one account, so a precise reason
      // ("this link expired") leaks nothing and is the difference between
      // retrying and giving up.
      setFormError(err instanceof Error ? err.message : "Couldn’t reset your password. Please try again.");
    } finally {
      setSubmitting(false);
    }
  };

  // A dead or absent token: say so and route back to the one screen that can
  // mint a fresh one, rather than leaving the user on a form that cannot work.
  if (!token || linkError) {
    return (
      <div>
        <div className="grid size-11 place-items-center rounded-xl bg-destructive/10 text-destructive">
          <LinkIcon className="size-5" />
        </div>
        <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
          This reset link isn’t valid
        </h1>
        <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
          Reset links work once and expire after an hour. Request a new one and
          use the most recent link you receive.
        </p>
        <div className="mt-7 grid gap-2">
          <Button nativeButton={false} className="w-full" render={<Link href="/forgot" />}>
            Request a new link
          </Button>
          <Button
            nativeButton={false}
            variant="ghost"
            className="w-full"
            render={<Link href="/login" />}
          >
            Back to log in
          </Button>
        </div>
      </div>
    );
  }

  if (done) {
    return (
      <div>
        <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
          <CheckCircle2 className="size-5" />
        </div>
        <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
          Password updated
        </h1>
        <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
          Your new password is active. Any other sessions on your account were
          signed out.
        </p>
        <div className="mt-7">
          <Button nativeButton={false} className="w-full" render={<Link href="/login" />}>
            Log in
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div>
      <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
        <KeyRound className="size-5" />
      </div>
      <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
        Choose a new password
      </h1>
      <p className="mt-1.5 text-sm text-muted-foreground">
        Pick something you haven’t used here before. You’ll use it to log in
        straight away.
      </p>

      <form className="mt-7 grid gap-4" onSubmit={submit} noValidate>
        <AuthField
          id="password"
          label="New password"
          type="password"
          autoComplete="new-password"
          placeholder="At least 8 characters"
          value={password}
          onValueChange={(v) => {
            setPassword(v);
            if (passwordError) setPasswordError(null);
            if (formError) setFormError(null);
          }}
          error={passwordError}
          autoFocus
        />
        <AuthField
          id="confirm"
          label="Confirm new password"
          type="password"
          autoComplete="new-password"
          value={confirm}
          onValueChange={(v) => {
            setConfirm(v);
            if (confirmError) setConfirmError(null);
          }}
          error={confirmError}
        />

        {formError && (
          <p role="alert" className="text-xs text-destructive">
            {formError}
          </p>
        )}

        <Button type="submit" className="w-full" disabled={submitting}>
          {submitting && <Loader2 className="size-4 animate-spin" />}
          Set new password
        </Button>
      </form>

      <Link
        href="/login"
        className="mt-6 inline-flex items-center gap-1.5 text-sm text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:text-foreground"
      >
        <ArrowLeft className="size-3.5" />
        Back to log in
      </Link>
    </div>
  );
}

export default function ResetPasswordPage() {
  return (
    <React.Suspense
      fallback={
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" />
          Checking your link…
        </div>
      }
    >
      <ResetPasswordForm />
    </React.Suspense>
  );
}
