"use client";

import * as React from "react";
import Link from "next/link";
import { ArrowLeft, Loader2, MailCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import { AuthField } from "@/components/auth/auth-field";
import { validateEmail } from "@/components/auth/validators";

export default function ForgotPasswordPage() {
  const [email, setEmail] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [submitting, setSubmitting] = React.useState(false);
  const [sent, setSent] = React.useState(false);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const emailError = validateEmail(email);
    setError(emailError);
    if (emailError) return;

    setSubmitting(true);
    // Mock reset-email dispatch → success state.
    setTimeout(() => {
      setSubmitting(false);
      setSent(true);
    }, 650);
  };

  if (sent) {
    return (
      <div>
        <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
          <MailCheck className="size-5" />
        </div>
        <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
          Check your inbox
        </h1>
        <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
          If an account exists for{" "}
          <span className="font-medium text-foreground">{email}</span>, we&apos;ve
          sent a link to reset your password. It may take a minute to arrive.
        </p>

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
          Didn&apos;t get the email? Check your spam folder or try again.
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
        Enter the email tied to your account and we&apos;ll send you a reset
        link.
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
