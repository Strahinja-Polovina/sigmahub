"use client";

import * as React from "react";
import Link from "next/link";
import {
  Globe,
  Loader2,
  Plus,
  ShieldCheck,
  ShieldAlert,
  Clock,
  Trash2,
  TriangleAlert,
} from "lucide-react";
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
import { DnsSetupDialog } from "./dns-setup-dialog";

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

function AddDomainForm({
  orgId,
  resourceId,
  edgeRoleMissing,
}: {
  orgId: string;
  resourceId: string;
  /** True when this resource's server is known NOT to carry the proxy/edge
   *  role. The success toast used to promise a certificate unconditionally;
   *  on such a server that promise cannot be kept (SIGMA-316). */
  edgeRoleMissing: boolean;
}) {
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
          description: edgeRoleMissing
            ? "No certificate will be issued until this resource's server is given the Proxy / edge role."
            : "sigmahub will issue a certificate once the domain's DNS points here.",
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
  server,
}: {
  orgId: string;
  resourceId: string;
  domains: DomainRow[];
  canManage: boolean;
  /** The server this resource runs on, when we know it and know its edge role.
   *  `proxyRole` is optional on purpose: demo mode and cluster workloads reach
   *  this panel without a role we can vouch for, and warning there would cry
   *  wolf on every domain. */
  server?: { id: string; name: string; proxyRole?: boolean } | null;
}) {
  const [pending, startTransition] = React.useTransition();
  const [dnsFor, setDnsFor] = React.useState<DomainRow | null>(null);

  // SIGMA-316: attaching a domain to a resource on a server with no proxy/edge
  // role is a promise the product cannot keep. The reconciler renders Traefik —
  // and with it the ACME client that requests the certificate — only onto
  // proxy-role servers, so on any other host the certificate is never even
  // requested. Nothing in the attach path used to say so: the form's toast
  // promised issuance "once the domain's DNS points here", the operator set up
  // DNS correctly, and the domain stayed pending forever. Explicitly `=== false`
  // so an unknown role stays silent.
  const edgeRoleMissing = server?.proxyRole === false;

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
          certificate automatically and serves it over HTTPS. Use{" "}
          <span className="font-medium text-foreground">DNS setup</span> on a domain to see the
          exact record to create and whether it has taken effect.
        </CardDescription>
      </CardHeader>
      <CardContent className="pt-4">
        {edgeRoleMissing && server && (
          <div className="mb-4 flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-500" />
            <div className="min-w-0 text-sm">
              <p className="font-medium text-amber-700 dark:text-amber-500">
                {server.name} is not an edge server, so no certificate will be issued for a
                domain here
              </p>
              <p className="mt-0.5 text-xs text-muted-foreground">
                Only a server carrying the proxy/edge role runs the proxy that answers the
                Let&apos;s Encrypt challenge, so DNS pointing here is not enough on its own.
                Turn on{" "}
                <span className="font-medium text-foreground">Proxy / edge role</span> on{" "}
                <Link
                  href={`/dashboard/servers/${server.id}`}
                  className="font-medium text-foreground underline underline-offset-2"
                >
                  {server.name}
                </Link>
                , or move this resource to a server that has it.
              </p>
            </div>
          </div>
        )}
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
                <Button
                  variant="outline"
                  size="sm"
                  className="ml-auto h-7"
                  onClick={() => setDnsFor(d)}
                >
                  <Globe className="size-3.5" />
                  DNS setup
                </Button>
                {canManage && (
                  <Button
                    variant="ghost"
                    size="icon-sm"
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
        {canManage && (
          <AddDomainForm
            orgId={orgId}
            resourceId={resourceId}
            edgeRoleMissing={edgeRoleMissing}
          />
        )}
      </CardContent>

      {dnsFor && (
        <DnsSetupDialog
          orgId={orgId}
          resourceId={resourceId}
          domainId={dnsFor.id}
          domain={dnsFor.domain}
          open
          onOpenChange={(o: boolean) => !o && setDnsFor(null)}
        />
      )}
    </Card>
  );
}
