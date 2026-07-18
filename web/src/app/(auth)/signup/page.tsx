"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";

import { authClient } from "@/lib/auth-client";
import { destAfterAuth } from "@/lib/after-auth";
import { Button } from "@/components/ui/button";
import { AuthField } from "@/components/auth/auth-field";
import { AuthDivider } from "@/components/auth/auth-divider";
import { SocialButtons } from "@/components/auth/social-buttons";
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
    const { error } = await authClient.signUp.email({ name, email, password });
    setSubmitting(false);

    if (error) {
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
    // signUp signs the user in — the session cookie is already set.
    toast.success("Account created", {
      description: "Welcome to SigmaHub. Your first 3 servers are free.",
    });
    router.push(destAfterAuth());
  };

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

      <div className="my-6">
        <AuthDivider label="or sign up with" />
      </div>

      <SocialButtons action="sign up" />

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
