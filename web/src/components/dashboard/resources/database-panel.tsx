"use client";

import * as React from "react";
import { Copy, Database, Eye, EyeOff, Loader2, Lock } from "lucide-react";
import { toast } from "sonner";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { revealDatabaseConnection } from "@/server/actions/databases";
import type { CpDatabaseInfo, CpDatabaseConnection } from "@/server/cp";

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

function copy(value: string, label: string) {
  void navigator.clipboard.writeText(value).then(
    () => toast.success(`${label} copied`),
    () => toast.error(`Couldn’t copy ${label.toLowerCase()}`)
  );
}

function ConnRow({ label, value, mono = true, copyable = false }: {
  label: string;
  value: string;
  mono?: boolean;
  copyable?: boolean;
}) {
  return (
    <div className="flex items-center justify-between gap-4 py-2">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className="inline-flex min-w-0 items-center gap-1.5">
        <span className={`truncate text-sm ${mono ? "font-mono" : ""}`}>{value}</span>
        {copyable && (
          <Button
            variant="ghost"
            size="icon"
            className="size-6 shrink-0"
            onClick={() => copy(value, label)}
            aria-label={`Copy ${label}`}
          >
            <Copy className="size-3.5" />
          </Button>
        )}
      </span>
    </div>
  );
}

/** P1-10 database connection panel: mesh-only connection metadata for every
 *  member, plus the audited credential reveal for Project Admin+. The backup
 *  section surfaces the policy row and warns loudly when no backup target is
 *  configured (an unrestored backup counts as no backup — P1-11 owns targets
 *  and execution). */
export function DatabasePanel({
  orgId,
  resourceId,
  info,
  canManage,
}: {
  orgId: string;
  resourceId: string;
  info: CpDatabaseInfo;
  canManage: boolean;
}) {
  const [conn, setConn] = React.useState<CpDatabaseConnection | null>(null);
  const [revealed, setRevealed] = React.useState(false);
  const [pending, startTransition] = React.useTransition();

  function reveal() {
    if (conn) {
      setRevealed((v) => !v);
      return;
    }
    startTransition(async () => {
      try {
        const c = await revealDatabaseConnection({ orgId, resourceId });
        setConn(c);
        setRevealed(true);
        toast.message("Credentials revealed", {
          description: "This reveal was written to the audit log.",
        });
      } catch (err) {
        toast.error("Couldn’t reveal credentials", { description: errMsg(err) });
      }
    });
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="inline-flex items-center gap-2">
              <Database className="size-4" />
              Connection
            </CardTitle>
            <CardDescription>
              Reachable only across your org’s WireGuard mesh — the port is never
              published on a public interface.
            </CardDescription>
          </div>
          <Badge variant="outline" className="shrink-0">
            <Lock className="size-3" />
            Mesh-only
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="flex flex-col divide-y divide-border">
          <ConnRow label="Engine" value={info.image} />
          <ConnRow
            label="Host"
            value={info.host || "pending mesh enrollment"}
            copyable={Boolean(info.host)}
          />
          <ConnRow label="Port" value={String(info.port)} copyable />
          {info.database && <ConnRow label="Database" value={info.database} copyable />}
          {info.username && <ConnRow label="Username" value={info.username} copyable />}
          <div className="flex items-center justify-between gap-4 py-2">
            <span className="text-sm text-muted-foreground">Password</span>
            <span className="inline-flex min-w-0 items-center gap-1.5">
              <span className="truncate font-mono text-sm">
                {revealed && conn ? conn.password : "••••••••••••"}
              </span>
              {revealed && conn && (
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-6 shrink-0"
                  onClick={() => copy(conn.password, "Password")}
                  aria-label="Copy password"
                >
                  <Copy className="size-3.5" />
                </Button>
              )}
              {canManage ? (
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7"
                  onClick={reveal}
                  disabled={pending}
                >
                  {pending ? (
                    <Loader2 className="size-3.5 animate-spin" />
                  ) : revealed ? (
                    <EyeOff className="size-3.5" />
                  ) : (
                    <Eye className="size-3.5" />
                  )}
                  {revealed ? "Hide" : "Reveal"}
                </Button>
              ) : (
                <span className="text-xs text-muted-foreground">Project Admin+</span>
              )}
            </span>
          </div>
          {revealed && conn && (
            <div className="flex items-center justify-between gap-4 py-2">
              <span className="text-sm text-muted-foreground">URL</span>
              <span className="inline-flex min-w-0 items-center gap-1.5">
                <span className="truncate font-mono text-sm">{conn.url}</span>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-6 shrink-0"
                  onClick={() => copy(conn.url, "Connection URL")}
                  aria-label="Copy connection URL"
                >
                  <Copy className="size-3.5" />
                </Button>
              </span>
            </div>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          Public exposure (per-engine TLS + IP allowlist) is not available in v1 —
          the API returns a typed not-enabled error by design.
        </p>
      </CardContent>
    </Card>
  );
}
