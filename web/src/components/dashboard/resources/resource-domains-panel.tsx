"use client";

import * as React from "react";
import { Globe, Loader2, Plus, ShieldCheck, ShieldAlert, Clock, Trash2 } from "lucide-react";
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
import { attachDomain, detachDomain } from "@/server/actions/domains";

export type DomainRow = {
  id: string;
  domain: string;
  certStatus: string;
  certExpiresAt?: string;
  lastError?: string;
};

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

function CertBadge({ status }: { status: string }) {
  const map: Record<string, { label: string; cls: string; icon: React.ElementType }> = {
    issued: { label: "Certificate issued", cls: "border-emerald-500/30 text-emerald-700", icon: ShieldCheck },
    issuing: { label: "Issuing…", cls: "border-amber-500/30 text-amber-700", icon: Clock },
    pending: { label: "Pending DNS", cls: "border-muted-foreground/30 text-muted-foreground", icon: Clock },
    failed: { label: "Failed", cls: "border-red-500/30 text-red-700", icon: ShieldAlert },
  };
  const m = map[status] ?? map.pending;
  const Icon = m.icon;
  return (
    <span className={`inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium ${m.cls}`}>
      <Icon className="size-3" />
      {m.label}
    </span>
  );
}

function AddDomainForm({ orgId, resourceId }: { orgId: string; resourceId: string }) {
  const [domain, setDomain] = React.useState("");
  const [pending, startTransition] = React.useTransition();

  function add() {
    if (!domain.includes(".")) {
      toast.error("Enter a valid domain (e.g. app.example.com).");
      return;
    }
    startTransition(async () => {
      try {
        await attachDomain({ orgId, resourceId, domain: domain.trim() });
        toast.success(`Attached ${domain.trim()}`, {
          description: "sigmahub will issue a certificate once the domain's DNS points here.",
        });
        setDomain("");
      } catch (err) {
        toast.error("Couldn’t attach domain", { description: errMsg(err) });
      }
    });
  }

  return (
    <div className="flex gap-2 pt-3">
      <Input
        placeholder="app.example.com"
        value={domain}
        onChange={(e) => setDomain(e.target.value)}
        autoComplete="off"
        className="h-9"
      />
      <Button size="sm" onClick={add} disabled={pending} className="h-9">
        {pending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
        Attach
      </Button>
    </div>
  );
}

export function ResourceDomainsPanel({
  orgId,
  resourceId,
  domains,
  canManage,
}: {
  orgId: string;
  resourceId: string;
  domains: DomainRow[];
  canManage: boolean;
}) {
  const [pending, startTransition] = React.useTransition();

  function remove(d: DomainRow) {
    startTransition(async () => {
      try {
        await detachDomain({ orgId, resourceId, domainId: d.id, domain: d.domain });
        toast.success(`Detached ${d.domain}`);
      } catch (err) {
        toast.error("Couldn’t detach domain", { description: errMsg(err) });
      }
    });
  }

  return (
    <Card>
      <CardHeader className="border-b">
        <CardTitle className="flex items-center gap-2 text-sm">
          <Globe className="size-4 text-muted-foreground" />
          Custom domains
        </CardTitle>
        <CardDescription>
          Point a domain’s DNS at this server; sigmahub’s proxy issues and renews a Let’s Encrypt
          certificate automatically and serves it over HTTPS.
        </CardDescription>
      </CardHeader>
      <CardContent className="pt-4">
        {domains.length === 0 ? (
          <p className="text-sm text-muted-foreground">No custom domains attached.</p>
        ) : (
          <ul className="flex flex-col divide-y divide-border">
            {domains.map((d) => (
              <li key={d.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2.5 first:pt-0">
                <span className="font-mono text-sm text-foreground">{d.domain}</span>
                <CertBadge status={d.certStatus} />
                {d.certExpiresAt && d.certStatus === "issued" && (
                  <span className="text-xs text-muted-foreground">
                    expires {new Date(d.certExpiresAt).toLocaleDateString("en-GB")}
                  </span>
                )}
                {d.lastError && d.certStatus === "failed" && (
                  <span className="text-xs text-red-700" title={d.lastError}>
                    {d.lastError.slice(0, 60)}
                  </span>
                )}
                {canManage && (
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    className="ml-auto"
                    aria-label={`Detach ${d.domain}`}
                    disabled={pending}
                    onClick={() => remove(d)}
                  >
                    <Trash2 className="size-3.5" />
                  </Button>
                )}
              </li>
            ))}
          </ul>
        )}
        {canManage && <AddDomainForm orgId={orgId} resourceId={resourceId} />}
      </CardContent>
    </Card>
  );
}
