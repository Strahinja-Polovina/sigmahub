"use client";

import * as React from "react";
import { Activity, CheckCircle2, Loader2, Rocket, Server, ShieldCheck } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { getBetaMetrics } from "@/server/actions/telemetry";
import type { CpBetaMetrics } from "@/server/cp";

function Stat({
  icon: Icon,
  label,
  value,
  hint,
}: {
  icon: React.ElementType;
  label: string;
  value: string;
  hint?: string;
}) {
  return (
    <div className="flex flex-col gap-1 rounded-lg border border-border p-4">
      <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
        <Icon className="size-3.5" />
        {label}
      </span>
      <span className="text-2xl font-semibold tabular-nums text-foreground">{value}</span>
      {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  );
}

/** M1 exit-criteria instrumentation (P1-13): deploy success rate over the
 *  last 500 deploys, restore-verify green streak, connected servers and the
 *  org's time-to-first-deploy — checkable at any time, per the gate. */
export function BetaMetricsTab({
  orgId,
  orgCreatedAt,
}: {
  orgId: string;
  orgCreatedAt: string | Date | null;
}) {
  const [metrics, setMetrics] = React.useState<CpBetaMetrics | null | undefined>(undefined);

  React.useEffect(() => {
    let cancelled = false;
    getBetaMetrics({ orgId })
      .then((m) => !cancelled && setMetrics(m))
      .catch(() => !cancelled && setMetrics(null));
    return () => {
      cancelled = true;
    };
  }, [orgId]);

  if (metrics === undefined) {
    return (
      <Card>
        <CardContent className="flex items-center justify-center py-12">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </CardContent>
      </Card>
    );
  }
  if (metrics === null) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Beta metrics</CardTitle>
          <CardDescription>
            Requires the control plane (set SIGMAHUB_CP_URL).
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  const rate = metrics.deploys.total > 0 ? `${(metrics.deploys.rate * 100).toFixed(1)}%` : "—";
  // TTFD (org leg): org creation → first successful deploy. The fleet-wide
  // median lives with the operator, joining every org's number.
  let ttfd = "—";
  if (metrics.firstDeployAt && orgCreatedAt) {
    const ms = new Date(metrics.firstDeployAt).getTime() - new Date(orgCreatedAt).getTime();
    if (ms > 0) {
      const mins = Math.round(ms / 60000);
      ttfd = mins < 120 ? `${mins} min` : `${Math.round(mins / 60)} h`;
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="inline-flex items-center gap-2">
          <Activity className="size-4" />
          Beta metrics
        </CardTitle>
        <CardDescription>
          The M1 exit-criteria feed: ≥95% deploy success over the last 500
          deploys, restore-verify green for 30 consecutive days, TTFD &lt; 15
          minutes.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Stat
          icon={Rocket}
          label="Deploy success rate"
          value={rate}
          hint={`${metrics.deploys.succeeded}/${metrics.deploys.total} of last ${metrics.deploys.window}`}
        />
        <Stat
          icon={ShieldCheck}
          label="Restore-verify streak"
          value={`${metrics.verifyStreakDays} d`}
          hint="consecutive green days (gate: 30)"
        />
        <Stat
          icon={CheckCircle2}
          label="Time to first deploy"
          value={ttfd}
          hint="org creation → first live deploy"
        />
        <Stat
          icon={Server}
          label="Connected servers"
          value={String(metrics.connectedServers)}
          hint="live agents right now"
        />
      </CardContent>
    </Card>
  );
}
