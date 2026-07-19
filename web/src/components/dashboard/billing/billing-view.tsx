"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  CreditCard,
  Server as ServerIcon,
  Check,
  Download,
  Info,
  ShieldCheck,
  Cpu,
  Boxes,
  Database,
  Archive,
  History,
} from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { StatusDot } from "@/components/dashboard/status-indicator";
import type { ServerType, Status } from "@/lib/mock";

const SERVER_TYPE_LABELS: Record<ServerType, string> = {
  general: "General",
  database: "Database",
  storage: "Storage",
  gpu: "GPU",
};

type Billing = {
  connected: number;
  freeTier: number;
  unitPrice: number;
  currency: string;
  amount: number;
  isFree: boolean;
};

type ServerItem = {
  id: string;
  name: string;
  type: string;
  region: string;
  status: string;
  byoVpn: boolean;
};

function money(amount: number, currency: string, withCents = false) {
  return new Intl.NumberFormat("en-IE", {
    style: "currency",
    currency,
    minimumFractionDigits: withCents ? 2 : 0,
    maximumFractionDigits: withCents ? 2 : 0,
  }).format(amount);
}

function currentPeriod() {
  const now = new Date();
  const first = new Date(now.getFullYear(), now.getMonth(), 1);
  const last = new Date(now.getFullYear(), now.getMonth() + 1, 0);
  const fmt = (d: Date, withYear = false) =>
    d.toLocaleDateString("en-GB", {
      day: "numeric",
      month: "short",
      ...(withYear ? { year: "numeric" } : {}),
    });
  return `${fmt(first)} – ${fmt(last, true)}`;
}

const INCLUDED_FEATURES: { label: string; icon: React.ElementType }[] = [
  { label: "Disaster recovery", icon: ShieldCheck },
  { label: "GPU & LLM hosting", icon: Cpu },
  { label: "Managed Kubernetes", icon: Boxes },
  { label: "Databases & Redis", icon: Database },
  { label: "S3 object storage", icon: Archive },
  { label: "Automated backups", icon: History },
];

/** Paddle subscription state (P2-4); present only in CP mode. */
type Subscription = {
  configured: boolean;
  status: string; // none | active | past_due | canceled
  billableServers: number;
  serverHoursThisMonth: number;
  orgId: string;
};

/** CP-mode subscription state + Paddle checkout/portal actions. Renders the
 *  honest not-configured banner when Paddle isn't wired. */
function SubscriptionCard({ sub }: { sub: Subscription }) {
  const [pending, startTransition] = React.useTransition();

  function go(kind: "checkout" | "portal") {
    startTransition(async () => {
      try {
        const { startCheckout, openBillingPortal } = await import("@/server/actions/billing");
        const res =
          kind === "checkout"
            ? await startCheckout(sub.orgId)
            : await openBillingPortal(sub.orgId);
        const url = "checkoutUrl" in res ? res.checkoutUrl : res.portalUrl;
        window.location.href = url;
      } catch (err) {
        toast.error("Couldn’t open billing", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  if (!sub.configured) {
    return (
      <Card>
        <CardContent className="flex items-start gap-2 py-4">
          <Info className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <p className="text-sm text-muted-foreground">
            Payments are not configured on this control plane, so the figures below are a
            usage preview only — no charges are made. An operator can wire Paddle
            (<span className="font-mono text-xs">CP_PADDLE_*</span>) to enable checkout and
            invoicing.
          </p>
        </CardContent>
      </Card>
    );
  }

  const statusLabel: Record<string, { text: string; cls: string }> = {
    none: { text: "No subscription", cls: "text-muted-foreground" },
    active: { text: "Active", cls: "text-emerald-700 dark:text-emerald-400" },
    past_due: { text: "Payment past due", cls: "text-destructive" },
    canceled: { text: "Canceled", cls: "text-amber-700 dark:text-amber-400" },
  };
  const st = statusLabel[sub.status] ?? statusLabel.none;

  return (
    <Card>
      <CardContent className="flex flex-wrap items-center justify-between gap-3 py-4">
        <div className="flex flex-col gap-0.5">
          <span className="text-sm font-medium text-foreground">
            Subscription: <span className={st.cls}>{st.text}</span>
          </span>
          <span className="text-xs text-muted-foreground tabular-nums">
            {sub.billableServers} billable {sub.billableServers === 1 ? "server" : "servers"} ·{" "}
            {sub.serverHoursThisMonth} connected server-hours this month
          </span>
          {sub.status === "past_due" && (
            <span className="text-xs text-destructive">
              Your servers keep running during the grace period — update your payment method to
              avoid interruption.
            </span>
          )}
        </div>
        <div className="flex items-center gap-2">
          {sub.status === "active" || sub.status === "past_due" ? (
            <Button variant="outline" size="sm" onClick={() => go("portal")} disabled={pending}>
              Manage subscription
            </Button>
          ) : (
            <Button
              size="sm"
              onClick={() => go("checkout")}
              disabled={pending || sub.billableServers < 1}
            >
              {sub.billableServers < 1 ? "Within free tier" : "Subscribe"}
            </Button>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

export function BillingView({
  orgName,
  billing,
  servers,
  subscription,
}: {
  orgName: string;
  billing: Billing;
  servers: ServerItem[];
  /** CP-mode Paddle state; omitted in demo mode. */
  subscription?: Subscription;
}) {
  const { unitPrice, freeTier, currency } = billing;
  const fc = (a: number, cents = false) => money(a, currency, cents);

  // Billing counts RUNNING servers only — the CP/Paddle charge basis (SIGMA-91).
  // gross (invoiceTotal) − free-tier credit == billing.amount (Total due), so the
  // preview stays internally consistent above the free tier.
  const connectedServers = servers.filter((s) => s.status === "running");
  const provisioningCount = servers.length - connectedServers.length;
  const freeUsed = Math.min(billing.connected, freeTier);
  const freeRemaining = Math.max(0, freeTier - billing.connected);
  const billableCount = Math.max(0, billing.connected - freeTier);
  const invoiceTotal = connectedServers.length * unitPrice;

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">Billing</h1>
        <p className="text-sm text-muted-foreground">
          One simple meter for {orgName}: you pay only for connected servers.
        </p>
      </div>

      {subscription && <SubscriptionCard sub={subscription} />}

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-1">
          <CardHeader>
            <CardDescription className="flex items-center justify-between">
              <span>Current monthly cost</span>
              <CreditCard className="size-4 text-muted-foreground" />
            </CardDescription>
            <CardTitle className="text-4xl tabular-nums tracking-tight">
              {fc(billing.amount)}
              <span className="ml-1 align-baseline text-base font-normal text-muted-foreground">
                /mo
              </span>
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            {billing.isFree ? (
              <Badge variant="outline" className="w-fit gap-1.5">
                <StatusDot status="running" />
                Free tier
              </Badge>
            ) : (
              <p className="text-sm text-muted-foreground tabular-nums">
                {billing.connected} connected × {fc(unitPrice)} per server
              </p>
            )}
            <div className="rounded-lg bg-muted/60 px-3 py-2 text-xs text-muted-foreground">
              {billing.isFree ? (
                <>
                  Free while you run {freeTier} or fewer connected servers. Beyond
                  that, every connected server is billed {fc(unitPrice)}/mo.
                </>
              ) : (
                <>
                  {billing.connected} connected{" "}
                  {billing.connected === 1 ? "server" : "servers"} × {fc(unitPrice)} ={" "}
                  <span className="font-medium text-foreground">{fc(billing.amount)}</span>{" "}
                  / month
                </>
              )}
            </div>
          </CardContent>
        </Card>

        <Card className="lg:col-span-1">
          <CardHeader>
            <CardDescription className="flex items-center justify-between">
              <span>Free tier</span>
              <ServerIcon className="size-4 text-muted-foreground" />
            </CardDescription>
            <CardTitle className="text-2xl tabular-nums">
              {billing.isFree ? (
                <>
                  {freeUsed} <span className="text-muted-foreground">of</span> {freeTier}
                </>
              ) : (
                <>
                  All {billing.connected} <span className="text-muted-foreground">billed</span>
                </>
              )}
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <Progress value={(freeUsed / freeTier) * 100} />
            <div className="flex items-center justify-between text-xs text-muted-foreground">
              <span>
                {billing.isFree
                  ? freeRemaining > 0
                    ? `${freeRemaining} free ${freeRemaining === 1 ? "server" : "servers"} remaining`
                    : "Free tier full"
                  : `Above the ${freeTier}-server free tier`}
              </span>
              <span
                className={
                  billableCount > 0 ? "font-medium text-foreground tabular-nums" : "tabular-nums"
                }
              >
                {billableCount} billable
              </span>
            </div>
            {provisioningCount > 0 && (
              <p className="text-xs text-muted-foreground">
                {provisioningCount} provisioning{" "}
                {provisioningCount === 1 ? "server is" : "servers are"} not billed until
                connected.
              </p>
            )}
          </CardContent>
        </Card>

        <Card className="lg:col-span-1">
          <CardHeader>
            <CardDescription className="flex items-center justify-between">
              <span>How pricing works</span>
              <Info className="size-4 text-muted-foreground" />
            </CardDescription>
            <CardTitle className="text-base leading-snug">One meter. No add-ons.</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <p className="text-sm text-muted-foreground">
              The only thing we count is your number of connected servers, at a flat{" "}
              {fc(unitPrice)} each per month. Every capability is included:
            </p>
            <ul className="grid grid-cols-2 gap-x-3 gap-y-1.5">
              {INCLUDED_FEATURES.map(({ label, icon: Icon }) => (
                <li key={label} className="flex items-center gap-1.5 text-xs text-foreground">
                  <Icon className="size-3.5 shrink-0 text-muted-foreground" />
                  {label}
                </li>
              ))}
            </ul>
            <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Check className="size-3.5 shrink-0 text-emerald-600" />
              No per-seat, per-request, or egress charges.
            </p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4 border-b">
          <div className="grid gap-1">
            <CardTitle>Invoice preview</CardTitle>
            <CardDescription>Current period · {currentPeriod()}</CardDescription>
          </div>
          <Button
            variant="outline"
            size="sm"
            className="gap-1.5"
            onClick={() =>
              toast.success("Invoice download started", {
                description: `${orgName} · ${currentPeriod()}`,
              })
            }
          >
            <Download className="size-3.5" />
            Download invoice
          </Button>
        </CardHeader>
        <CardContent className="px-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Connected server</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Region</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead className="pr-4 text-right">Amount</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {connectedServers.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="py-8 text-center text-sm text-muted-foreground">
                    No connected servers this period.
                  </TableCell>
                </TableRow>
              ) : (
                connectedServers.map((sv) => (
                  <InvoiceRow key={sv.id} server={sv} unitPrice={unitPrice} currency={currency} />
                ))
              )}
            </TableBody>
            <TableFooter>
              <TableRow>
                <TableCell colSpan={3} className="pl-4 text-muted-foreground">
                  {connectedServers.length}{" "}
                  {connectedServers.length === 1 ? "server" : "servers"} × {fc(unitPrice)}
                  {billing.isFree && (
                    <span className="text-muted-foreground/70"> · free tier applied</span>
                  )}
                </TableCell>
                <TableCell className="text-right text-muted-foreground tabular-nums">
                  {connectedServers.length}
                </TableCell>
                <TableCell className="pr-4 text-right font-medium tabular-nums">
                  {fc(invoiceTotal, true)}
                </TableCell>
              </TableRow>
              {freeUsed > 0 && invoiceTotal > 0 && (
                <TableRow>
                  <TableCell colSpan={4} className="pl-4 text-muted-foreground">
                    Free tier credit ({freeUsed} × {fc(unitPrice)})
                  </TableCell>
                  <TableCell className="pr-4 text-right text-muted-foreground tabular-nums">
                    −{fc(freeUsed * unitPrice, true)}
                  </TableCell>
                </TableRow>
              )}
              <TableRow>
                <TableCell colSpan={4} className="pl-4 font-medium text-foreground">
                  Total due
                </TableCell>
                <TableCell className="pr-4 text-right text-base font-semibold tabular-nums">
                  {fc(billing.amount, true)}
                </TableCell>
              </TableRow>
            </TableFooter>
          </Table>
        </CardContent>
      </Card>

      <p className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
        <ShieldCheck className="size-3.5 shrink-0" />
        Payments are processed by <span className="font-medium text-foreground">Paddle</span> (our
        merchant of record — it handles EU VAT/sales tax and invoicing). This is a preview; invoices
        are issued at the end of each billing period based on the servers connected during it.
      </p>
    </div>
  );
}

function InvoiceRow({
  server,
  unitPrice,
  currency,
}: {
  server: ServerItem;
  unitPrice: number;
  currency: string;
}) {
  return (
    <TableRow>
      <TableCell className="pl-4 font-medium text-foreground">
        <span className="inline-flex items-center gap-2">
          <StatusDot status={server.status as Status} />
          {server.name}
          {server.byoVpn && (
            <Badge variant="outline" className="font-normal">
              BYO VPN
            </Badge>
          )}
        </span>
      </TableCell>
      <TableCell>
        <Badge variant="outline" className="font-mono">
          {SERVER_TYPE_LABELS[server.type as ServerType]}
        </Badge>
      </TableCell>
      <TableCell className="text-muted-foreground">{server.region}</TableCell>
      <TableCell className="text-right text-muted-foreground tabular-nums">1</TableCell>
      <TableCell className="pr-4 text-right tabular-nums">
        {money(unitPrice, currency, true)}
      </TableCell>
    </TableRow>
  );
}
