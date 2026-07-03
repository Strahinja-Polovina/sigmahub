"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { ArrowLeft, Loader2, ShieldCheck } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { AuthField } from "@/components/auth/auth-field";
import { AuthDivider } from "@/components/auth/auth-divider";
import { OtpInput } from "@/components/auth/otp-input";
import { SocialButtons } from "@/components/auth/social-buttons";
import {
  validateEmail,
  validateOtp,
  validatePassword,
} from "@/components/auth/validators";

type Step = "credentials" | "totp";

export default function LoginPage() {
  const router = useRouter();
  const [step, setStep] = React.useState<Step>("credentials");

  // Credentials state
  const [email, setEmail] = React.useState("");
  const [password, setPassword] = React.useState("");
  const [errors, setErrors] = React.useState<{
    email?: string | null;
    password?: string | null;
  }>({});
  const [submitting, setSubmitting] = React.useState(false);

  // TOTP state
  const [code, setCode] = React.useState("");
  const [codeError, setCodeError] = React.useState<string | null>(null);
  const [verifying, setVerifying] = React.useState(false);

  const submitCredentials = (e: React.FormEvent) => {
    e.preventDefault();
    const emailError = validateEmail(email);
    const passwordError = validatePassword(password);
    setErrors({ email: emailError, password: passwordError });
    if (emailError || passwordError) return;

    setSubmitting(true);
    // Mock credential check → advance to the 2FA step.
    setTimeout(() => {
      setSubmitting(false);
      setStep("totp");
      toast.info("Verification required", {
        description: "Enter the 6-digit code from your authenticator app.",
      });
    }, 550);
  };

  const verifyCode = (value?: string) => {
    const candidate = value ?? code;
    const err = validateOtp(candidate);
    setCodeError(err);
    if (err) return;

    setVerifying(true);
    // Mock TOTP verification → into the app.
    setTimeout(() => {
      toast.success("Welcome back");
      router.push("/dashboard");
    }, 650);
  };

  if (step === "totp") {
    return (
      <div>
        <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
          <ShieldCheck className="size-5" />
        </div>
        <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
          Two-factor authentication
        </h1>
        <p className="mt-1.5 text-sm text-muted-foreground">
          Enter the 6-digit code from your authenticator app for{" "}
          <span className="font-medium text-foreground">{email}</span>.
        </p>

        <form
          className="mt-7 grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            verifyCode();
          }}
        >
          <OtpInput
            value={code}
            onChange={(v) => {
              setCode(v);
              if (codeError) setCodeError(null);
            }}
            onComplete={(v) => verifyCode(v)}
            invalid={!!codeError}
            disabled={verifying}
            autoFocus
          />
          {codeError && <p className="text-xs text-destructive">{codeError}</p>}

          <Button type="submit" className="w-full" disabled={verifying}>
            {verifying && <Loader2 className="size-4 animate-spin" />}
            Verify and continue
          </Button>
        </form>

        <div className="mt-6 flex items-center justify-between text-sm">
          <button
            type="button"
            onClick={() => {
              setStep("credentials");
              setCode("");
              setCodeError(null);
            }}
            className="inline-flex items-center gap-1.5 text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:text-foreground"
          >
            <ArrowLeft className="size-3.5" />
            Back
          </button>
          <button
            type="button"
            onClick={() =>
              toast.info("Code resent", {
                description: "A new code is on its way to your device.",
              })
            }
            className="font-medium text-primary outline-none transition-colors hover:text-primary/80 focus-visible:underline"
          >
            Resend code
          </button>
        </div>
      </div>
    );
  }

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight text-foreground">
        Log in to SigmaHub
      </h1>
      <p className="mt-1.5 text-sm text-muted-foreground">
        Welcome back. Enter your details to continue.
      </p>

      <form className="mt-7 grid gap-4" onSubmit={submitCredentials} noValidate>
        <AuthField
          id="email"
          label="Email"
          type="email"
          autoComplete="email"
          placeholder="you@company.com"
          value={email}
          onValueChange={(v) => {
            setEmail(v);
            if (errors.email) setErrors((p) => ({ ...p, email: null }));
          }}
          error={errors.email}
          autoFocus
        />

        <AuthField
          id="password"
          label="Password"
          type="password"
          autoComplete="current-password"
          placeholder="••••••••"
          value={password}
          onValueChange={(v) => {
            setPassword(v);
            if (errors.password) setErrors((p) => ({ ...p, password: null }));
          }}
          error={errors.password}
          labelAside={
            <Link
              href="/forgot"
              className="text-xs font-medium text-primary outline-none transition-colors hover:text-primary/80 focus-visible:underline"
            >
              Forgot password?
            </Link>
          }
        />

        <Button type="submit" className="w-full" disabled={submitting}>
          {submitting && <Loader2 className="size-4 animate-spin" />}
          Continue
        </Button>
      </form>

      <div className="my-6">
        <AuthDivider label="or continue with" />
      </div>

      <SocialButtons action="continue" />

      <p className="mt-8 text-center text-sm text-muted-foreground">
        Don&apos;t have an account?{" "}
        <Link
          href="/signup"
          className="font-medium text-primary outline-none transition-colors hover:text-primary/80 focus-visible:underline"
        >
          Sign up
        </Link>
      </p>
    </div>
  );
}
