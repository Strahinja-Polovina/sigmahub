"use client";

import * as React from "react";
import { Copy, KeyRound, Loader2, ShieldCheck, ShieldOff } from "lucide-react";
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

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

/** TOTP 2FA enrollment (closes the "server plugin exists, no UI" gap):
 *  password → otpauth secret + backup codes → verify one code → enabled. */
export function SecurityTab({ initialTwoFactorEnabled }: { initialTwoFactorEnabled: boolean }) {
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

  return (
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
}
