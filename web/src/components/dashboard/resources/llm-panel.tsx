"use client";

import * as React from "react";
import { Copy, Cpu, Lock } from "lucide-react";
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
import type { CpLLMInfo } from "@/server/cp";

function copy(value: string, label: string) {
  void navigator.clipboard.writeText(value).then(
    () => toast.success(`${label} copied`),
    () => toast.error(`Couldn’t copy ${label.toLowerCase()}`)
  );
}

function InfoRow({ label, value, copyable = false }: {
  label: string;
  value: string;
  copyable?: boolean;
}) {
  return (
    <div className="flex items-center justify-between gap-4 py-2">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className="inline-flex min-w-0 items-center gap-1.5">
        <span className="truncate font-mono text-sm">{value}</span>
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

/**
 * SIGMA-303 — where a deployed model actually listens.
 *
 * The whole LLM path could be walked end to end — pick Llama-3.1-8B-Instruct,
 * pass the VRAM fit check, deploy to a GPU host, watch the container come up —
 * and the resource page then showed a green Running badge and nothing else. No
 * host, no port, no model id, no URL anywhere in the product. The port is not
 * guessable either: the control plane allocates it from MESH_PORT_BASE upward,
 * so a user who did everything right had a running model they could not reach.
 *
 * The endpoint is deliberately the first row and the one with the prominent copy
 * button: it is the single value someone leaves this page with, pasted into an
 * OpenAI client's base_url.
 */
export function LlmPanel({ info }: { info: CpLLMInfo }) {
  const reachable = Boolean(info.host);
  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="inline-flex items-center gap-2">
              <Cpu className="size-4" />
              Inference endpoint
            </CardTitle>
            <CardDescription>
              An OpenAI-compatible API, reachable only across your org’s WireGuard
              mesh — point any OpenAI client’s base URL at it.
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
          <div className="flex items-center justify-between gap-4 py-2">
            <span className="text-sm text-muted-foreground">Endpoint</span>
            <span className="inline-flex min-w-0 items-center gap-1.5">
              <span className="truncate font-mono text-sm">
                {reachable ? info.endpoint : "pending mesh enrollment"}
              </span>
              {reachable && info.endpoint && (
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 shrink-0"
                  onClick={() => copy(info.endpoint, "Endpoint")}
                  aria-label="Copy Endpoint"
                >
                  <Copy className="size-3.5" />
                  Copy
                </Button>
              )}
            </span>
          </div>
          <InfoRow label="Model" value={info.model} copyable={Boolean(info.model)} />
          <InfoRow label="Runtime" value={info.engine} />
          <InfoRow label="Image" value={info.image || "—"} />
          <InfoRow
            label="Host"
            value={info.host || "pending mesh enrollment"}
            copyable={reachable}
          />
          <InfoRow label="Port" value={String(info.port)} copyable={info.port > 0} />
        </div>

        <p className="text-xs text-muted-foreground">
          The port is allocated by the control plane, not chosen by you — it is
          stable for the lifetime of this resource. Nothing is published on a
          public interface: reach it from another host on the mesh, or from an app
          in the same project.
        </p>
      </CardContent>
    </Card>
  );
}
