"use client";

import * as React from "react";
import { Copy, KeyRound, Loader2, Lock, ShieldCheck, ShieldOff } from "lucide-react";
import { toast } from "sonner";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { authClient } from "@/lib/auth-client";
import { validateConfirm, validatePassword } from "@/components/auth/validators";

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

/** Change your own password (SIGMA-345).
 *
 *  There was no way to do this anywhere in the product: a password could not be
 *  rotated while signed in, and — until SIGMA-344 — could not be reset while
 *  locked out either, so the password typed at signup was the only one an
 *  account would ever have and a leaked one could not be retired.
 *
 *  revokeOtherSessions is the point of rotating after a leak rather than a
 *  nicety: without it the session opened with the stolen password outlives the
 *  change. better-auth re-issues a session for THIS browser in the same call,
 *  so the user is not signed out of the tab they are standing in. */
function PasswordCard() {
  const [current, setCurrent] = React.useState("");
  const [next, setNext] = React.useState("");
  const [confirm, setConfirm] = React.useState("");
  const [pending, setPending] = React.useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!current) {
      toast.error("Enter your current password.");
      return;
    }
    // Same rules as signup and /reset-password, from the same helpers, so the
    // three places a password is chosen cannot disagree about what is allowed.
    const nErr = validatePassword(next);
    const cErr = validateConfirm(next, confirm);
    if (nErr || cErr) {
      toast.error(nErr ?? cErr ?? "Check the new password.");
      return;
    }
    setPending(true);
    try {
      const res = await authClient.changePassword({
        currentPassword: current,
        newPassword: next,
        revokeOtherSessions: true,
      });
      if (res.error) throw new Error(res.error.message);
      setCurrent("");
      setNext("");
      setConfirm("");
      toast.success("Password changed", {
        description: "Other sessions on your account were signed out.",
      });
    } catch (err) {
      toast.error("Couldn’t change your password", { description: errMsg(err) });
    } finally {
      setPending(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="inline-flex items-center gap-2">
          <Lock className="size-4" />
          Password
        </CardTitle>
        <CardDescription>
          Changing it signs out every other session on your account.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form className="flex max-w-md flex-col gap-4" onSubmit={submit} noValidate>
          <div className="flex flex-col gap-2">
            <Label htmlFor="pw-current">Current password</Label>
            <Input
              id="pw-current"
              type="password"
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
              autoComplete="current-password"
            />
          </div>
          <div className="flex flex-col gap-2">
            <Label htmlFor="pw-new">New password</Label>
            <Input
              id="pw-new"
              type="password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
              autoComplete="new-password"
              placeholder="At least 8 characters"
            />
          </div>
          <div className="flex flex-col gap-2">
            <Label htmlFor="pw-confirm">Confirm new password</Label>
            <Input
              id="pw-confirm"
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
            />
          </div>
          <Button type="submit" size="sm" className="w-fit" disabled={pending}>
            {pending ? <Loader2 className="size-4 animate-spin" /> : <Lock className="size-4" />}
            Change password
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

/** Password rotation + TOTP 2FA enrollment (closes the "server plugin exists,
 *  no UI" gap): password → otpauth secret + backup codes → verify one code →
 *  enabled.
 *
 *  hasPassword is false for an account that only ever signed in through a social
 *  provider. changePassword has no stored credential to verify against there and
 *  answers CREDENTIAL_ACCOUNT_NOT_FOUND, so the card is hidden rather than shown
 *  as a form that cannot succeed. */
export function SecurityTab({
  initialTwoFactorEnabled,
  hasPassword = true,
}: {
  initialTwoFactorEnabled: boolean;
  hasPassword?: boolean;
}) {
  const [enabled, setEnabled] = React.useState(initialTwoFactorEnabled);
  const [password, setPassword] = React.useState("");
  const [totpUri, setTotpUri] = React.useState<string | null>(null);
  const [backupCodes, setBackupCodes] = React.useState<string[]>([]);
  const [code, setCode] = React.useState("");
  const [pending, setPending] = React.useState(false);

  async function begin() {
    if (!password) {
      toast.error("Enter your password to manage 2FA.");
      return;
    }
    setPending(true);
    try {
      const res = await authClient.twoFactor.enable({ password });
      if (res.error) throw new Error(res.error.message);
      setTotpUri(res.data?.totpURI ?? null);
      setBackupCodes(res.data?.backupCodes ?? []);
      toast.message("Scan the secret, then confirm with a code", {
        description: "2FA activates only after a successful code check.",
      });
    } catch (err) {
      toast.error("Couldn’t start enrollment", { description: errMsg(err) });
    } finally {
      setPending(false);
    }
  }

  async function confirm() {
    setPending(true);
    try {
      const res = await authClient.twoFactor.verifyTotp({ code });
      if (res.error) throw new Error(res.error.message);
      setEnabled(true);
      setTotpUri(null);
      setPassword("");
      setCode("");
      toast.success("Two-factor authentication enabled");
    } catch (err) {
      toast.error("Code didn’t verify", { description: errMsg(err) });
    } finally {
      setPending(false);
    }
  }

  async function disable() {
    if (!password) {
      toast.error("Enter your password to disable 2FA.");
      return;
    }
    setPending(true);
    try {
      const res = await authClient.twoFactor.disable({ password });
      if (res.error) throw new Error(res.error.message);
      setEnabled(false);
      setPassword("");
      toast.success("Two-factor authentication disabled");
    } catch (err) {
      toast.error("Couldn’t disable 2FA", { description: errMsg(err) });
    } finally {
      setPending(false);
    }
  }

  const secret = totpUri ? new URL(totpUri).searchParams.get("secret") : null;

  const twoFactorCard = (
    <Card>
      <CardHeader>
        <CardTitle className="inline-flex items-center gap-2">
          <KeyRound className="size-4" />
          Two-factor authentication
        </CardTitle>
        <CardDescription>
          TOTP codes from any authenticator app. Login asks for a code once
          enabled.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex max-w-md flex-col gap-4">
        <p className="inline-flex items-center gap-2 text-sm">
          {enabled ? (
            <>
              <ShieldCheck className="size-4 text-emerald-600" />
              <span className="text-foreground">2FA is enabled on your account.</span>
            </>
          ) : (
            <>
              <ShieldOff className="size-4 text-amber-600" />
              <span className="text-foreground">2FA is not enabled.</span>
            </>
          )}
        </p>

        {!totpUri && (
          <div className="flex flex-col gap-2">
            <Label htmlFor="sec-password">Account password</Label>
            <Input
              id="sec-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
            {enabled ? (
              <Button variant="destructive" size="sm" className="w-fit" onClick={disable} disabled={pending}>
                {pending ? <Loader2 className="size-4 animate-spin" /> : <ShieldOff className="size-4" />}
                Disable 2FA
              </Button>
            ) : (
              <Button size="sm" className="w-fit" onClick={begin} disabled={pending}>
                {pending ? <Loader2 className="size-4 animate-spin" /> : <ShieldCheck className="size-4" />}
                Enable 2FA
              </Button>
            )}
          </div>
        )}

        {totpUri && (
          <div className="flex flex-col gap-3">
            <div className="rounded-md border border-border bg-muted/40 p-3">
              <p className="text-xs font-medium text-muted-foreground">
                Add to your authenticator (secret)
              </p>
              <div className="mt-1 flex items-center gap-1.5">
                <code className="break-all font-mono text-sm">{secret ?? totpUri}</code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-6 shrink-0"
                  aria-label="Copy secret"
                  onClick={() =>
                    void navigator.clipboard
                      .writeText(secret ?? totpUri)
                      .then(() => toast.success("Secret copied"))
                  }
                >
                  <Copy className="size-3.5" />
                </Button>
              </div>
            </div>
            {backupCodes.length > 0 && (
              <div className="rounded-md border border-amber-500/40 bg-amber-500/10 p-3">
                <p className="text-xs font-medium text-amber-700 dark:text-amber-400">
                  Backup codes — store them now; they are shown once.
                </p>
                <div className="mt-1 grid grid-cols-2 gap-x-4 font-mono text-xs">
                  {backupCodes.map((c) => (
                    <span key={c}>{c}</span>
                  ))}
                </div>
              </div>
            )}
            <div className="flex items-end gap-2">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="sec-code">6-digit code</Label>
                <Input
                  id="sec-code"
                  value={code}
                  onChange={(e) => setCode(e.target.value.replace(/\D/g, "").slice(0, 6))}
                  inputMode="numeric"
                  className="w-32 font-mono"
                />
              </div>
              <Button size="sm" onClick={confirm} disabled={pending || code.length !== 6}>
                {pending ? <Loader2 className="size-4 animate-spin" /> : <ShieldCheck className="size-4" />}
                Confirm
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );

  return (
    <div className="grid gap-4">
      {hasPassword && <PasswordCard />}
      {twoFactorCard}
    </div>
  );
}
