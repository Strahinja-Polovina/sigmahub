"use client";

import * as React from "react";
import { toast } from "sonner";
import { Copy, Eye, EyeOff, Loader2, Network, ShieldCheck, CalendarClock } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { revealDBConnection } from "@/server/actions/databases";

export type BackupPolicyRow = {
  schedule: string;
  retentionDays: number;
  enabled: boolean;
} | null;

/** Connection panel for a database resource (P1-10). The connection string is
 *  revealed on demand — Project Admin+ only; every reveal is audited on the
 *  control plane. v1 databases are mesh-internal: the host is the server's
 *  WireGuard address, unreachable from the public internet. */
export function DBConnectionPanel({
  orgId,
  resourceId,
  canManage,
  backupPolicy,
}: {
  orgId: string;
  resourceId: string;
  canManage: boolean;
  backupPolicy: BackupPolicyRow;
}) {
  const [conn, setConn] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);

  async function reveal() {
    setBusy(true);
    try {
      const res = await revealDBConnection({ orgId, resourceId });
      setConn(res.connectionString);
      toast.info("Connection revealed", { description: "This access was audited." });
    } catch (err) {
      toast.error("Couldn’t reveal connection", {
        description: err instanceof Error ? err.message : "Please try again.",
      });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <CardHeader className="border-b">
        <div className="flex items-center justify-between gap-4">
          <div className="flex flex-col gap-1">
            <CardTitle>Connection</CardTitle>
            <CardDescription>
              Mesh-internal only — reachable from your servers across the private WireGuard mesh,
              never from the public internet.
            </CardDescription>
          </div>
          <Badge variant="secondary" className="inline-flex items-center gap-1">
            <Network className="size-3" />
            mesh
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="flex flex-col gap-4 pt-4">
        <div className="flex flex-col gap-2">
          <span className="text-xs font-medium text-muted-foreground">Connection string</span>
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 truncate rounded-md border border-border bg-muted/40 px-3 py-2 font-mono text-xs">
              {conn ?? "••••••••••••••••••••••••••••••••"}
            </code>
            {conn ? (
              <>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    void navigator.clipboard.writeText(conn);
                    toast.success("Copied");
                  }}
                >
                  <Copy className="size-3.5" />
                </Button>
                <Button variant="outline" size="sm" onClick={() => setConn(null)}>
                  <EyeOff className="size-3.5" />
                </Button>
              </>
            ) : canManage ? (
              <Button variant="outline" size="sm" disabled={busy} onClick={reveal}>
                {busy ? <Loader2 className="size-3.5 animate-spin" /> : <Eye className="size-3.5" />}
                Reveal
              </Button>
            ) : (
              <Badge variant="outline" className="inline-flex items-center gap-1 text-muted-foreground">
                <ShieldCheck className="size-3" />
                Project Admin only
              </Badge>
            )}
          </div>
          {conn && (
            <p className="text-xs text-muted-foreground">
              Revealed values are audited. Treat this string as a secret.
            </p>
          )}
        </div>

        {backupPolicy && (
          <div className="flex items-center justify-between gap-4 rounded-md border border-border bg-muted/20 px-3 py-2">
            <span className="inline-flex items-center gap-2 text-sm text-muted-foreground">
              <CalendarClock className="size-4" />
              Backups
            </span>
            <span className="text-sm text-foreground">
              <code className="font-mono text-xs">{backupPolicy.schedule}</code>
              {" · "}
              {backupPolicy.retentionDays}d retention
              {" · "}
              {backupPolicy.enabled ? "enabled" : "disabled"}
            </span>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
