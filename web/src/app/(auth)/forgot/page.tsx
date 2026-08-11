"use client";

import * as React from "react";
import Link from "next/link";
import { ArrowLeft, Loader2, MailCheck, ServerCog } from "lucide-react";

import { authClient } from "@/lib/auth-client";
import { Button } from "@/components/ui/button";
import { AuthField } from "@/components/auth/auth-field";
import { useMailDelivery } from "@/components/auth/mail-delivery";
import { validateEmail } from "@/components/auth/validators";

export default function ForgotPasswordPage() {
  const [email, setEmail] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [submitting, setSubmitting] = React.useState(false);
  const [sent, setSent] = React.useState(false);
  // SIGMA-307: whether this deployment can deliver the mail we just asked for.
  // No SMTP transport is bundled, so on a self-hosted install the reset link
  // goes to the web container's stdout — a place the locked-out user has no
  // reason to look at and probably no access to. Telling them to check their
  // inbox, and then their spam folder, is the difference between "ask your
  // admin for the link" and a day of searching for mail that was never sent.
  const mailDelivered = useMailDelivery();

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const emailError = validateEmail(email);
    setError(emailError);
    if (emailError) return;

    setSubmitting(true);
    // Best-effort dispatch. We always show the same confirmation regardless of
    // the outcome so the form never reveals whether an account exists — what
    // the confirmation SAYS depends only on the deployment's mail transport,
    // which is not a fact about this email address.
    try {
      // redirectTo is where better-auth's own GET /reset-password/<token>
      // bounces the browser once it has checked the token is live — it carries
      // ?token=… (or ?error=INVALID_TOKEN) into whatever page this names, and
      // sets no password itself. Pointing it at /login sent the user to a form
      // that reads neither, so the link resolved to a dead end and recovery was
      // impossible (SIGMA-344).
      await authClient.requestPasswordReset({ email, redirectTo: "/reset-password" });
    } catch {
      // swallow — don't leak account existence
    }
    setSubmitting(false);
    setSent(true);
  };

  if (sent) {
    return (
      <div>
        <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
          {mailDelivered ? <MailCheck className="size-5" /> : <ServerCog className="size-5" />}
        </div>
        <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
          {mailDelivered ? "Check your inbox" : "Email delivery isn’t configured"}
        </h1>
        {mailDelivered ? (
          <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
            If an account exists for{" "}
            <span className="font-medium text-foreground">{email}</span>, we&apos;ve
            sent a link to reset your password. It may take a minute to arrive.
          </p>
        ) : (
          <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
            This deployment has no mail transport, so no email was sent. The reset
            link for{" "}
            <span className="font-medium text-foreground">{email}</span> was written
            to the dashboard server&apos;s log — ask your administrator for it.
          </p>
        )}

        <div className="mt-7 grid gap-2">
          <Button
            nativeButton={false}
            className="w-full"
            render={<Link href="/login" />}
          >
            Back to log in
          </Button>
          <Button
            type="button"
            variant="ghost"
            className="w-full"
            onClick={() => {
              setSent(false);
              setEmail("");
            }}
          >
            Use a different email
          </Button>
        </div>

        <p className="mt-6 text-center text-xs text-muted-foreground">
          {mailDelivered
            ? "Didn’t get the email? Check your spam folder or try again."
            : "An administrator can wire a mail transport so these links are sent automatically."}
        </p>
      </div>
    );
  }

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight text-foreground">
        Reset your password
      </h1>
      <p className="mt-1.5 text-sm text-muted-foreground">
        {mailDelivered
          ? "Enter the email tied to your account and we’ll send you a reset link."
          : "Enter the email tied to your account. This deployment sends no mail, so the link is written to the dashboard server’s log for an administrator to relay."}
      </p>

      <form className="mt-7 grid gap-4" onSubmit={submit} noValidate>
        <AuthField
          id="email"
          label="Email"
          type="email"
          autoComplete="email"
          placeholder="you@company.com"
          value={email}
          onValueChange={(v) => {
            setEmail(v);
            if (error) setError(null);
          }}
          error={error}
          autoFocus
        />

        <Button type="submit" className="w-full" disabled={submitting}>
          {submitting && <Loader2 className="size-4 animate-spin" />}
          Send reset link
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
