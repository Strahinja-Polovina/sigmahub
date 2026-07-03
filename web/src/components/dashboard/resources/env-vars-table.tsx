"use client";

import * as React from "react";
import { toast } from "sonner";
import { Eye, EyeOff, Copy, KeyRound, Lock } from "lucide-react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
  DialogClose,
} from "@/components/ui/dialog";

type Secret = {
  key: string;
  value: string;
  secret: boolean;
};

// A deterministic mock secret set, seeded off the resource id so it's stable.
function buildSecrets(seedKey: string): Secret[] {
  let h = 0;
  for (const c of seedKey) h = (h * 31 + c.charCodeAt(0)) % 9973;
  const token = h.toString(16).padStart(6, "0");
  return [
    { key: "NODE_ENV", value: "production", secret: false },
    { key: "PORT", value: "3000", secret: false },
    { key: "LOG_LEVEL", value: "info", secret: false },
    {
      key: "DATABASE_URL",
      value: `postgres://app:${token}@db.internal:5432/app`,
      secret: true,
    },
    { key: "REDIS_URL", value: "redis://cache.internal:6379/0", secret: false },
    { key: "JWT_SECRET", value: `sk_live_${token}${token}`, secret: true },
    { key: "STRIPE_KEY", value: `rk_live_${token}9f2a`, secret: true },
  ];
}

function mask(value: string) {
  return "•".repeat(Math.min(Math.max(value.length, 8), 24));
}

export function EnvVarsTable({ seedKey }: { seedKey: string }) {
  const secrets = React.useMemo(() => buildSecrets(seedKey), [seedKey]);
  const [revealed, setRevealed] = React.useState<Record<string, boolean>>({});
  const [confirmKey, setConfirmKey] = React.useState<string | null>(null);

  function toggle(secret: Secret) {
    if (revealed[secret.key]) {
      setRevealed((r) => ({ ...r, [secret.key]: false }));
      return;
    }
    // Non-sensitive values reveal immediately; sensitive ones require confirmation.
    if (secret.secret) {
      setConfirmKey(secret.key);
    } else {
      setRevealed((r) => ({ ...r, [secret.key]: true }));
    }
  }

  function confirmReveal() {
    if (confirmKey) {
      setRevealed((r) => ({ ...r, [confirmKey]: true }));
      setConfirmKey(null);
    }
  }

  return (
    <>
      <div className="overflow-hidden rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-4">Key</TableHead>
              <TableHead>Value</TableHead>
              <TableHead className="w-28 pr-4 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {secrets.map((s) => {
              const isRevealed = Boolean(revealed[s.key]);
              return (
                <TableRow key={s.key}>
                  <TableCell className="pl-4">
                    <span className="inline-flex items-center gap-2 font-mono text-sm text-foreground">
                      {s.secret ? (
                        <Lock className="size-3.5 text-muted-foreground" />
                      ) : (
                        <KeyRound className="size-3.5 text-muted-foreground" />
                      )}
                      {s.key}
                      {s.secret && (
                        <Badge variant="outline" className="text-[10px]">
                          secret
                        </Badge>
                      )}
                    </span>
                  </TableCell>
                  <TableCell>
                    <span
                      className={
                        isRevealed
                          ? "font-mono text-sm text-foreground"
                          : "font-mono text-sm tracking-wider text-muted-foreground"
                      }
                    >
                      {isRevealed ? s.value : mask(s.value)}
                    </span>
                  </TableCell>
                  <TableCell className="pr-4 text-right">
                    <div className="inline-flex items-center gap-0.5">
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        aria-label={isRevealed ? "Hide value" : "Reveal value"}
                        onClick={() => toggle(s)}
                      >
                        {isRevealed ? (
                          <EyeOff className="size-4 text-muted-foreground" />
                        ) : (
                          <Eye className="size-4 text-muted-foreground" />
                        )}
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        aria-label="Copy value"
                        onClick={() => toast.success(`Copied ${s.key}`)}
                      >
                        <Copy className="size-4 text-muted-foreground" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>

      <Dialog
        open={confirmKey !== null}
        onOpenChange={(o) => !o && setConfirmKey(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Reveal secret value?</DialogTitle>
            <DialogDescription>
              You're about to display the plaintext value of{" "}
              <span className="font-mono text-foreground">{confirmKey}</span>.
              Make sure no one is looking over your shoulder.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
            <Button onClick={confirmReveal}>
              <Eye className="size-4" />
              Reveal
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
