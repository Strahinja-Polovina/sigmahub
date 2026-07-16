"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  Loader2,
  RotateCcw,
  ScrollText,
  GitCommitHorizontal,
  ChevronRight,
} from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { DeployStatusBadge } from "./deploy-status-badge";
import { formatDateTime, formatDuration } from "./resource-meta";
import { rollbackDeployment, fetchDeployLogs } from "@/server/actions/deployments";

export type DeploymentRow = {
  id: string;
  trigger: string;
  gitRef?: string;
  gitSha?: string;
  status: string;
  detail?: string;
  rollbackOf?: string;
  imageDigest?: string;
  buildSeconds?: number;
  durationSeconds?: number;
  createdBy?: string;
  createdAt: string;
  startedAt?: string;
  serviceStatus?: Record<string, string>;
};

type LogLine = { id: number; stream: string; line: string; at: string };

const TERMINAL = new Set(["success", "failed", "superseded", "rolled_back"]);
const shortSha = (sha?: string) => (sha ? sha.slice(0, 7) : "—");

/** Live build/orchestration log for a single deployment. Polls the CP through a
 *  server action while the deploy is in-flight, then stops once terminal. */
function DeployLogs({
  orgId,
  deploymentId,
  initiallyLive,
}: {
  orgId: string;
  deploymentId: string;
  initiallyLive: boolean;
}) {
  const [lines, setLines] = React.useState<LogLine[]>([]);
  const [done, setDone] = React.useState(!initiallyLive);
  const [loading, setLoading] = React.useState(true);
  const cursorRef = React.useRef(0);
  const bottomRef = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let failures = 0;
    const MAX_FAILURES = 5;
    // The CP caps each page at 1000 lines; a full page means more may remain.
    const PAGE = 1000;

    async function poll() {
      try {
        const snap = await fetchDeployLogs({ orgId, deploymentId, after: cursorRef.current });
        if (cancelled) return;
        failures = 0;
        if (snap.logs.length > 0) {
          cursorRef.current = snap.nextCursor;
          setLines((prev) => [...prev, ...snap.logs]);
        }
        setLoading(false);
        // Even when the deployment is terminal, keep draining while a full page
        // came back — otherwise the failing tail past 1000 lines is dropped. Only
        // stop once terminal AND the last page was not full.
        if (snap.done && snap.logs.length < PAGE) {
          setDone(true);
          return;
        }
        // Drain a backlog immediately; poll live tails on an interval.
        timer = setTimeout(poll, snap.logs.length >= PAGE ? 0 : 1500);
      } catch {
        if (cancelled) return;
        setLoading(false);
        // A transient fetch error must not permanently kill live streaming or
        // falsely mark the deploy finished — retry with backoff, give up only
        // after several consecutive failures.
        failures += 1;
        if (failures >= MAX_FAILURES) {
          setDone(true);
          return;
        }
        timer = setTimeout(poll, 1500 * failures);
      }
    }
    poll();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [orgId, deploymentId]);

  React.useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [lines.length]);

  return (
    <div className="overflow-hidden rounded-lg border border-border bg-muted/30">
      <div className="max-h-[360px] overflow-y-auto p-3 font-mono text-xs leading-relaxed">
        {loading && lines.length === 0 ? (
          <div className="flex items-center gap-2 text-muted-foreground">
            <Loader2 className="size-3.5 animate-spin" />
            Loading logs…
          </div>
        ) : lines.length === 0 ? (
          <div className="text-muted-foreground">No build output recorded.</div>
        ) : (
          lines.map((l) => (
            <div key={l.id} className="flex items-start gap-3 whitespace-pre-wrap py-0.5">
              <span className="w-16 shrink-0 uppercase text-muted-foreground/70">{l.stream}</span>
              <span className="min-w-0 flex-1 text-foreground">{l.line}</span>
            </div>
          ))
        )}
        <div ref={bottomRef} />
      </div>
      {!done && (
        <div className="flex items-center gap-2 border-t border-border bg-card px-3 py-1.5 text-xs text-muted-foreground">
          <Loader2 className="size-3 animate-spin" />
          Streaming…
        </div>
      )}
    </div>
  );
}

function RollbackButton({
  orgId,
  resourceId,
  targetId,
  disabled,
}: {
  orgId: string;
  resourceId: string;
  targetId: string;
  disabled?: boolean;
}) {
  const router = useRouter();
  const [busy, setBusy] = React.useState(false);
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={busy || disabled}
      onClick={async () => {
        setBusy(true);
        try {
          await rollbackDeployment({ orgId, resourceId, targetDeploymentId: targetId });
          toast.success("Rollback queued", {
            description: "Re-shipping the retained image — no rebuild.",
          });
          router.refresh();
        } catch (err) {
          toast.error("Rollback failed", {
            description: err instanceof Error ? err.message : "Please try again.",
          });
        } finally {
          setBusy(false);
        }
      }}
    >
      {busy ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCcw className="size-3.5" />}
      Rollback
    </Button>
  );
}

export function DeploymentsPanel({
  orgId,
  resourceId,
  deployments,
  rollbackTargetIds,
  canManage,
}: {
  orgId: string;
  resourceId: string;
  deployments: DeploymentRow[];
  /** Deployment ids that are rebuild-free rollback candidates. */
  rollbackTargetIds: string[];
  canManage: boolean;
}) {
  const [openId, setOpenId] = React.useState<string | null>(null);
  const targets = React.useMemo(() => new Set(rollbackTargetIds), [rollbackTargetIds]);
  // The current release is the newest successful (or in-flight) deployment; a
  // rollback to it would be a no-op, so it's never offered as a target.
  const currentId = deployments.find((d) => d.status === "success")?.id;

  if (deployments.length === 0) {
    return (
      <Card>
        <CardHeader className="border-b">
          <CardTitle>Deployments</CardTitle>
          <CardDescription>Release history &amp; build logs</CardDescription>
        </CardHeader>
        <CardContent className="py-10 text-center text-sm text-muted-foreground">
          No deployments yet. Push to the connected branch, or use Deploy above.
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader className="border-b">
        <CardTitle>Deployments</CardTitle>
        <CardDescription>
          Release history — each row is an immutable build. Roll back to any prior successful
          release without a rebuild.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col divide-y divide-border px-0 py-0">
        {deployments.map((d) => {
          const live = !TERMINAL.has(d.status);
          const isOpen = openId === d.id;
          const canRollback =
            canManage && targets.has(d.id) && d.id !== currentId && d.status === "success";
          return (
            <div key={d.id} className="flex flex-col">
              <div className="flex items-center gap-3 px-4 py-3">
                <button
                  type="button"
                  onClick={() => setOpenId(isOpen ? null : d.id)}
                  className="flex min-w-0 flex-1 items-center gap-3 text-left"
                  aria-expanded={isOpen}
                >
                  <ChevronRight
                    className={cn(
                      "size-4 shrink-0 text-muted-foreground transition-transform",
                      isOpen && "rotate-90"
                    )}
                  />
                  <GitCommitHorizontal className="size-4 shrink-0 text-muted-foreground" />
                  <span className="font-mono text-sm text-foreground">{shortSha(d.gitSha)}</span>
                  <DeployStatusBadge status={d.status} />
                  {d.trigger === "rollback" && (
                    <Badge variant="secondary" className="text-[10px]">
                      rollback
                    </Badge>
                  )}
                  {d.trigger === "git" && d.gitRef && (
                    <span className="hidden truncate text-xs text-muted-foreground sm:inline">
                      {d.gitRef.replace("refs/heads/", "")}
                    </span>
                  )}
                </button>
                <span className="hidden shrink-0 text-xs text-muted-foreground md:inline">
                  {d.createdBy ?? "—"}
                </span>
                <span className="hidden w-16 shrink-0 text-right text-xs tabular-nums text-muted-foreground sm:inline">
                  {d.durationSeconds != null ? formatDuration(d.durationSeconds) : live ? "…" : "—"}
                </span>
                <span className="hidden shrink-0 text-right text-xs tabular-nums text-muted-foreground lg:inline">
                  {formatDateTime(d.startedAt ?? d.createdAt)}
                </span>
                {canRollback && (
                  <RollbackButton orgId={orgId} resourceId={resourceId} targetId={d.id} />
                )}
              </div>
              {isOpen && (
                <div className="flex flex-col gap-3 bg-muted/20 px-4 pb-4 pt-1">
                  {d.detail && (
                    <p
                      className={cn(
                        "text-xs",
                        d.status === "failed" ? "text-red-600" : "text-muted-foreground"
                      )}
                    >
                      {d.detail}
                    </p>
                  )}
                  {d.serviceStatus && Object.keys(d.serviceStatus).length > 0 && (
                    <div className="flex flex-col gap-1.5">
                      <span className="text-xs font-medium text-muted-foreground">Services</span>
                      <div className="flex flex-wrap gap-2">
                        {Object.entries(d.serviceStatus)
                          .sort(([a], [b]) => a.localeCompare(b))
                          .map(([svc, st]) => (
                            <span
                              key={svc}
                              className="inline-flex items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs"
                            >
                              <span className="font-mono text-foreground">{svc}</span>
                              <DeployStatusBadge status={st} />
                            </span>
                          ))}
                      </div>
                    </div>
                  )}
                  <div className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                    <ScrollText className="size-3.5" />
                    Build &amp; orchestration log
                  </div>
                  <DeployLogs orgId={orgId} deploymentId={d.id} initiallyLive={live} />
                </div>
              )}
            </div>
          );
        })}
      </CardContent>
    </Card>
  );
}
