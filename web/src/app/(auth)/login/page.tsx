"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { ArrowLeft, Loader2, MailCheck, ServerCog, ShieldCheck } from "lucide-react";
import { toast } from "sonner";

import { authClient } from "@/lib/auth-client";
import { destAfterAuth } from "@/lib/after-auth";
import { Button } from "@/components/ui/button";
import { AuthField } from "@/components/auth/auth-field";
import { AuthDivider } from "@/components/auth/auth-divider";
import { OtpInput } from "@/components/auth/otp-input";
import { SocialButtons } from "@/components/auth/social-buttons";
import { useAuthProviders } from "@/components/auth/auth-providers";
import { useMailDelivery } from "@/components/auth/mail-delivery";
import { anyAuthProvider } from "@/lib/auth-providers";
import {
  validateEmail,
  validateOtp,
  validatePassword,
} from "@/components/auth/validators";

type Step = "credentials" | "totp" | "verify";

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

  const ssoAvailable = anyAuthProvider(useAuthProviders());
  // Whether the link this screen may have just re-sent reaches a mailbox at all.
  const mailDelivered = useMailDelivery();

  // TOTP state
  const [code, setCode] = React.useState("");
  const [codeError, setCodeError] = React.useState<string | null>(null);
  const [verifying, setVerifying] = React.useState(false);

  // Verification state
  const [resending, setResending] = React.useState(false);

  const submitCredentials = async (e: React.FormEvent) => {
    e.preventDefault();
    const emailError = validateEmail(email);
    const passwordError = validatePassword(password);
    setErrors({ email: emailError, password: passwordError });
    if (emailError || passwordError) return;

    setSubmitting(true);
    const { data, error } = await authClient.signIn.email({
      email,
      password,
      callbackURL: destAfterAuth(),
    });
    setSubmitting(false);

    if (error) {
      // The password was right; the address has not been confirmed. Rendering
      // this as "Invalid email or password" — which is what the generic branch
      // below did — sends the user to /forgot to reset a password that is not
      // the problem, and the reset does not lift the block either, so recovery is
      // impossible from inside the product (SIGMA-365). The server has just
      // re-issued the link (emailVerification.sendOnSignIn), so this screen only
      // has to say where it went.
      if (
        error.status === 403 ||
        error.code === "EMAIL_NOT_VERIFIED" ||
        /not verified/i.test(error.message ?? "")
      ) {
        setStep("verify");
        return;
      }
      toast.error("Couldn’t sign in", {
        description: error.message ?? "Invalid email or password.",
      });
      return;
    }
    // Users with 2FA enrolled get a redirect flag instead of a session.
    if ((data as { twoFactorRedirect?: boolean } | null)?.twoFactorRedirect) {
      setStep("totp");
      toast.info("Verification required", {
        description: "Enter the 6-digit code from your authenticator app.",
      });
      return;
    }
    toast.success("Welcome back");
    router.push(destAfterAuth());
  };

  const verifyCode = async (value?: string) => {
    const candidate = value ?? code;
    const err = validateOtp(candidate);
    setCodeError(err);
    if (err) return;

    setVerifying(true);
    const { error } = await authClient.twoFactor.verifyTotp({ code: candidate });
    setVerifying(false);

    if (error) {
      setCodeError("Invalid or expired code. Try again.");
      return;
    }
    toast.success("Welcome back");
    router.push(destAfterAuth());
  };

  const resendVerification = async () => {
    setResending(true);
    try {
      await authClient.sendVerificationEmail({ email, callbackURL: destAfterAuth() });
    } catch {
      // Swallowed: the outcome must not differ between an address that exists
      // and one that does not.
    }
    setResending(false);
    toast.success(
      mailDelivered ? "Verification email sent" : "Verification link written to the log"
    );
  };

  if (step === "verify") {
    return (
      <div>
        <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
          {mailDelivered ? <MailCheck className="size-5" /> : <ServerCog className="size-5" />}
        </div>
        <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
          {mailDelivered ? "Confirm your email to continue" : "Email delivery isn’t configured"}
        </h1>
        {mailDelivered ? (
          <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
            Your password is correct, but{" "}
            <span className="font-medium text-foreground">{email}</span> hasn&apos;t been
            confirmed yet. We&apos;ve just sent a fresh link — open it, then come back
            and log in.
          </p>
        ) : (
          <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
            Your password is correct, but{" "}
            <span className="font-medium text-foreground">{email}</span> hasn&apos;t been
            confirmed and this deployment sends no mail. The link was written to the
            dashboard server&apos;s log — ask your administrator for it.
          </p>
        )}

        <div className="mt-7 grid gap-2">
          <Button
            type="button"
            className="w-full"
            onClick={resendVerification}
            disabled={resending}
          >
            {resending && <Loader2 className="size-4 animate-spin" />}
            {mailDelivered ? "Send it again" : "Write a new link to the log"}
          </Button>
          <Button
            type="button"
            variant="ghost"
            className="w-full"
            onClick={() => {
              setStep("credentials");
              setPassword("");
            }}
          >
            Use a different account
          </Button>
        </div>

        <p className="mt-6 text-center text-xs text-muted-foreground">
          {mailDelivered
            ? "Didn’t get it? Check your spam folder, or send it again above."
            : "An administrator can wire a mail transport so these links are sent automatically."}
        </p>
      </div>
    );
  }

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

      {/* Only when a provider is actually configured — divider included
          (SIGMA-246). */}
      {ssoAvailable && (
        <>
          <div className="my-6">
            <AuthDivider label="or continue with" />
          </div>

          <SocialButtons action="continue" />
        </>
      )}

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
