"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { Loader2, MailCheck, ServerCog } from "lucide-react";
import { toast } from "sonner";

import { authClient } from "@/lib/auth-client";
import { destAfterAuth } from "@/lib/after-auth";
import { Button } from "@/components/ui/button";
import { AuthField } from "@/components/auth/auth-field";
import { AuthDivider } from "@/components/auth/auth-divider";
import { SocialButtons } from "@/components/auth/social-buttons";
import { useAuthProviders } from "@/components/auth/auth-providers";
import { useMailDelivery } from "@/components/auth/mail-delivery";
import { anyAuthProvider } from "@/lib/auth-providers";
import {
  validateConfirm,
  validateEmail,
  validateName,
  validatePassword,
} from "@/components/auth/validators";

type Errors = {
  name?: string | null;
  email?: string | null;
  password?: string | null;
  confirm?: string | null;
};

export default function SignupPage() {
  const router = useRouter();
  const [name, setName] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [password, setPassword] = React.useState("");
  const [confirm, setConfirm] = React.useState("");
  const [errors, setErrors] = React.useState<Errors>({});
  const [submitting, setSubmitting] = React.useState(false);
  // Set when sign-up returned no session because the address has to be verified
  // first. Holds the address, so the confirmation screen and the resend button
  // both name it back to the user.
  const [awaitingVerification, setAwaitingVerification] = React.useState<string | null>(
    null
  );
  const [resending, setResending] = React.useState(false);
  const ssoAvailable = anyAuthProvider(useAuthProviders());
  // Same switch /forgot reads: on a deployment with no transport the
  // verification link is a line in the web container's log, and the screen has
  // to say so rather than send the user to an inbox nothing will arrive in.
  const mailDelivered = useMailDelivery();

  const clear = (key: keyof Errors) =>
    setErrors((prev) => (prev[key] ? { ...prev, [key]: null } : prev));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const next: Errors = {
      name: validateName(name),
      email: validateEmail(email),
      password: validatePassword(password),
      confirm: validateConfirm(password, confirm),
    };
    setErrors(next);
    if (next.name || next.email || next.password || next.confirm) return;

    setSubmitting(true);
    // callbackURL is where better-auth's /verify-email sends the browser once
    // the link is used. destAfterAuth() carries an ?invite= token through, so a
    // teammate who signed up from an invite lands on the accept page rather than
    // an empty dashboard — the verification hop is invisible to them.
    const { data, error } = await authClient.signUp.email({
      name,
      email,
      password,
      callbackURL: destAfterAuth(),
    });
    setSubmitting(false);

    if (error) {
      // Only reachable when verification is OFF. With it on, better-auth answers
      // a duplicate address with the same 200 + null token a new sign-up gets,
      // deliberately, so the form cannot be used to enumerate accounts — which
      // is why the branch below must not claim the account was created.
      const isDup = error.status === 422 || /exist/i.test(error.message ?? "");
      setErrors((p) => ({
        ...p,
        email: isDup ? "An account with this email already exists." : p.email,
      }));
      toast.error("Couldn’t create account", {
        description: error.message ?? "Please try again.",
      });
      return;
    }

    // No token means no session: verification is required and better-auth has
    // just mailed the link (SIGMA-365). Pushing to the dashboard here — which is
    // what this page used to do unconditionally — bounced straight back to
    // /login through the (auth) layout's session check, after a toast saying the
    // account was ready. The user was left with working credentials, a screen
    // that said "welcome", and no way in.
    if (!data?.token) {
      setAwaitingVerification(email);
      return;
    }

    toast.success("Account created", {
      description: "Welcome to SigmaHub. Your first 3 servers are free.",
    });
    router.push(destAfterAuth());
  };

  const resend = async () => {
    if (!awaitingVerification) return;
    setResending(true);
    try {
      await authClient.sendVerificationEmail({
        email: awaitingVerification,
        callbackURL: destAfterAuth(),
      });
    } catch {
      // Swallowed for the same reason /forgot swallows: the outcome must not
      // differ between an address that exists and one that does not.
    }
    setResending(false);
    toast.success(mailDelivered ? "Verification email sent" : "Verification link written to the log");
  };

  if (awaitingVerification) {
    return (
      <div>
        <div className="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary">
          {mailDelivered ? <MailCheck className="size-5" /> : <ServerCog className="size-5" />}
        </div>
        <h1 className="mt-5 text-xl font-semibold tracking-tight text-foreground">
          {mailDelivered ? "Verify your email" : "Email delivery isn’t configured"}
        </h1>
        {mailDelivered ? (
          <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
            If{" "}
            <span className="font-medium text-foreground">{awaitingVerification}</span>{" "}
            is new to SigmaHub, a verification link is on its way. Open it to confirm
            the address, then log in with the password you just chose.
          </p>
        ) : (
          <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
            This deployment has no mail transport, so no email was sent. The
            verification link for{" "}
            <span className="font-medium text-foreground">{awaitingVerification}</span>{" "}
            was written to the dashboard server&apos;s log — ask your administrator for
            it. Sign-in stays blocked until the link is used.
          </p>
        )}

        <div className="mt-7 grid gap-2">
          <Button
            type="button"
            className="w-full"
            onClick={resend}
            disabled={resending}
          >
            {resending && <Loader2 className="size-4 animate-spin" />}
            {mailDelivered ? "Resend the link" : "Write a new link to the log"}
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

        <p className="mt-6 text-center text-xs text-muted-foreground">
          {mailDelivered
            ? "Didn’t get it? Check your spam folder, or resend above."
            : "An administrator can wire a mail transport so these links are sent automatically."}
        </p>
      </div>
    );
  }

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight text-foreground">
        Create your account
      </h1>
      <p className="mt-1.5 text-sm text-muted-foreground">
        Connect your first server in minutes. Up to 3 are free.
      </p>

      <form className="mt-7 grid gap-4" onSubmit={submit} noValidate>
        <AuthField
          id="name"
          label="Full name"
          type="text"
          autoComplete="name"
          placeholder="Ada Lovelace"
          value={name}
          onValueChange={(v) => {
            setName(v);
            clear("name");
          }}
          error={errors.name}
          autoFocus
        />
        <AuthField
          id="email"
          label="Work email"
          type="email"
          autoComplete="email"
          placeholder="you@company.com"
          value={email}
          onValueChange={(v) => {
            setEmail(v);
            clear("email");
          }}
          error={errors.email}
        />
        <AuthField
          id="password"
          label="Password"
          type="password"
          autoComplete="new-password"
          placeholder="At least 8 characters"
          value={password}
          onValueChange={(v) => {
            setPassword(v);
            clear("password");
          }}
          error={errors.password}
        />
        <AuthField
          id="confirm"
          label="Confirm password"
          type="password"
          autoComplete="new-password"
          placeholder="Re-enter your password"
          value={confirm}
          onValueChange={(v) => {
            setConfirm(v);
            clear("confirm");
          }}
          error={errors.confirm}
        />

        <Button type="submit" className="w-full" disabled={submitting}>
          {submitting && <Loader2 className="size-4 animate-spin" />}
          Create account
        </Button>
      </form>

      <p className="mt-4 text-center text-xs leading-relaxed text-muted-foreground">
        By creating an account you agree to the SigmaHub Terms and Privacy
        Policy.
      </p>

      {/* Only when a provider is actually configured — divider included, since
          an "or sign up with" rule over nothing is its own small lie
          (SIGMA-246). */}
      {ssoAvailable && (
        <>
          <div className="my-6">
            <AuthDivider label="or sign up with" />
          </div>

          <SocialButtons action="sign up" />
        </>
      )}

      <p className="mt-8 text-center text-sm text-muted-foreground">
        Already have an account?{" "}
        <Link
          href="/login"
          className="font-medium text-primary outline-none transition-colors hover:text-primary/80 focus-visible:underline"
        >
          Log in
        </Link>
      </p>
    </div>
  );
}
