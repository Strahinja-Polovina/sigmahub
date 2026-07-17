"use client";

import * as React from "react";
import { Loader2, ScrollText, Search } from "lucide-react";
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
import { LogsViewer } from "@/components/dashboard/resources/logs-viewer";
import { searchLogs } from "@/server/actions/telemetry";
import type { CpLogLine } from "@/server/cp";

/** P1-13 env-wide log search: one query bar over every resource in the
 *  environment, backed by the tenant-isolated Loki proxy. */
export function EnvLogSearch({ orgId, environmentId }: { orgId: string; environmentId: string }) {
  const [q, setQ] = React.useState("");
  const [logs, setLogs] = React.useState<CpLogLine[] | null>(null);
  const [searched, setSearched] = React.useState(false);
  const [pending, startTransition] = React.useTransition();

  function run() {
    startTransition(async () => {
      try {
        const out = await searchLogs({ orgId, environmentId, q: q.trim() || undefined });
        setLogs(out);
        setSearched(true);
      } catch (err) {
        toast.error("Log search failed", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="inline-flex items-center gap-2">
          <ScrollText className="size-4" />
          Logs
        </CardTitle>
        <CardDescription>
          Search runtime logs across every resource in this environment.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex gap-2">
          <Input
            placeholder="Filter lines (substring match)…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && run()}
            className="h-9"
          />
          <Button size="sm" className="h-9" onClick={run} disabled={pending}>
            {pending ? <Loader2 className="size-4 animate-spin" /> : <Search className="size-4" />}
            Search
          </Button>
        </div>
        {searched &&
          (logs === null ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              Telemetry pipeline not configured — set CP_LOKI_URL on the control
              plane to collect container logs.
            </p>
          ) : logs.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              No matching log lines in the last 24 hours.
            </p>
          ) : (
            <LogsViewer logs={logs} />
          ))}
      </CardContent>
    </Card>
  );
}
