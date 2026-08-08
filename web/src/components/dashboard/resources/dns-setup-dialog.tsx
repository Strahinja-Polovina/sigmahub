"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  CircleCheck,
  Copy,
  Globe,
  Loader2,
  RefreshCw,
  TriangleAlert,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { getDomainDNS } from "@/server/actions/dns";
import type { CpDNSSetup } from "@/server/cp";

/**
 * What to type into your registrar, and whether it has taken effect.
 *
 * Attaching a domain is only half the job — nothing routes until the record
 * exists — and the product used to say nothing about which record, pointing
 * where. The target is always the server's PUBLIC address: the mesh address is
 * the single most likely thing to be pasted in by mistake, and it resolves to
 * nothing from the internet.
 */
export function DnsSetupDialog({
  orgId,
  resourceId,
  domainId,
  domain,
  open,
  onOpenChange,
}: {
  orgId: string;
  resourceId: string;
  domainId: string;
  domain: string;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  // `checks` is bumped by "Check again"; the result carries the generation it
  // answered, so a re-check reads as loading until its own result lands rather
  // than showing the previous answer as if it were fresh.
  const [checks, setChecks] = React.useState(0);
  const [result, setResult] = React.useState<{ check: number; setup: CpDNSSetup | null } | null>(
    null
  );

  React.useEffect(() => {
    if (!open) return;
    let cancelled = false;

    async function load() {
      const res = await getDomainDNS({ orgId, resourceId, domainId });
      if (cancelled) return;
      setResult({ check: checks, setup: res });
    }
    void load();
    return () => {
      cancelled = true;
    };
  }, [open, orgId, resourceId, domainId, checks]);

  const fresh = result?.check === checks;
  const setup = fresh ? result?.setup ?? null : null;
  const loading = !fresh;

  function copy(value: string, label: string) {
    void navigator.clipboard.writeText(value).then(
      () => toast.success(`${label} copied`),
      () => toast.error(`Couldn’t copy ${label.toLowerCase()}`)
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle className="inline-flex items-center gap-2">
            <Globe className="size-4" />
            DNS for {domain}
          </DialogTitle>
          <DialogDescription>
            Create this record at whoever manages {domain}&apos;s DNS. Nothing routes —
            and no certificate is issued — until it exists.
          </DialogDescription>
        </DialogHeader>

        {loading && !setup ? (
          <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            Checking DNS…
          </div>
        ) : !setup ? (
          <p className="py-6 text-sm text-muted-foreground">
            Couldn&apos;t reach the control plane to check this domain. Try again in a
            moment.
          </p>
        ) : (
          <div className="flex flex-col gap-4 py-1">
            <div className="flex flex-wrap items-center gap-2">
              {setup.verified ? (
                <Badge variant="outline" className="gap-1.5 text-emerald-700 dark:text-emerald-400">
                  <CircleCheck className="size-3.5" />
                  DNS points here
                </Badge>
              ) : (
                <Badge variant="outline" className="gap-1.5 text-amber-700 dark:text-amber-500">
                  <TriangleAlert className="size-3.5" />
                  Not pointing here yet
                </Badge>
              )}
              <Badge variant="outline" className="text-[10px]">
                certificate: {setup.certStatus}
              </Badge>
              {setup.apex && (
                <Badge variant="outline" className="text-[10px]">
                  apex domain
                </Badge>
              )}
            </div>

            {setup.reason && (
              <p className="text-sm text-muted-foreground">{setup.reason}</p>
            )}

            {setup.records.length > 0 && (
              <div className="overflow-x-auto rounded-lg border border-border">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border text-xs text-muted-foreground">
                      <th className="px-3 py-2 text-left font-medium">Type</th>
                      <th className="px-3 py-2 text-left font-medium">Name</th>
                      <th className="px-3 py-2 text-left font-medium">Value</th>
                      <th className="px-3 py-2 text-right font-medium">TTL</th>
                      <th className="px-3 py-2" />
                    </tr>
                  </thead>
                  <tbody>
                    {setup.records.map((rec) => (
                      <tr key={`${rec.type}-${rec.name}`} className="border-b border-border last:border-0">
                        <td className="px-3 py-2 font-mono">{rec.type}</td>
                        <td className="px-3 py-2 font-mono">{rec.name}</td>
                        <td className="px-3 py-2 font-mono break-all">{rec.value}</td>
                        <td className="px-3 py-2 text-right font-mono tabular-nums">{rec.ttl}</td>
                        <td className="px-3 py-2 text-right">
                          <Button
                            variant="ghost"
                            size="icon"
                            className="size-7"
                            aria-label={`Copy ${rec.type} record value`}
                            onClick={() => copy(rec.value, `${rec.type} value`)}
                          >
                            <Copy className="size-3.5" />
                          </Button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            {setup.apex && setup.records.length > 0 && (
              <p className="text-xs text-muted-foreground">
                This is an apex domain, so it needs an {setup.records[0]?.type} record — a
                CNAME is not legal here. If your provider offers “ALIAS” or “ANAME”, that
                works too.
              </p>
            )}

            {setup.observed && setup.observed.length > 0 && !setup.verified && (
              <p className="text-xs text-muted-foreground">
                Currently resolving to{" "}
                <span className="font-mono text-foreground">{setup.observed.join(", ")}</span>.
              </p>
            )}

            <p className="text-xs text-muted-foreground">
              Checked {new Date(setup.checkedAt).toLocaleTimeString()}. DNS changes usually
              take a few minutes; some providers take up to an hour.
            </p>
          </div>
        )}

        <DialogFooter>
          <Button
            variant="outline"
            disabled={loading}
            onClick={() => setChecks((n) => n + 1)}
          >
            {loading ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
            Check again
          </Button>
          <DialogClose render={<Button />}>Done</DialogClose>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
