"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  AlertTriangle,
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

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import type { Status } from "@/lib/mock";
import { serverUnitWeight, type ServerUnitLine } from "@/lib/billing-units";
import { serverTypeLabel } from "@/lib/server-catalog.generated";


type Billing = {
  /** Connected SERVER count — what the fleet looks like. */
  connected: number;
  /** Weighted total across the servers connected right now. */
  units: number;
  /** Unit total the subscription is actually priced on: the high-water mark of
   *  `units` over `billingWindowHours` (SIGMA-292). Higher than `units` when the
   *  fleet shrank inside the window, and then it — not `units` — is the number
   *  Paddle invoices, so the page has to show it and say where it came from.
   *  Absent in demo mode, which has no meter and no subscription. */
  billedUnits?: number;
  billingWindowHours?: number;
  billableUnits: number;
  /** Per-server-type explanation of `units`. */
  breakdown: ServerUnitLine[];
  /** Free allowance, in units. */
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
  billableUnits: number;
  serverHoursThisMonth: number;
  orgId: string;
};

/** Send the browser to Paddle — checkout to subscribe, the customer portal for
 *  payment method, subscription state and, crucially, invoices.
 *
 *  Shared by the subscription card and the invoice-preview header: Paddle is the
 *  merchant of record, so it holds the only copy of the actual invoice document
 *  and the portal is the only place it can be fetched from. */
function useBillingPortal() {
  const [pending, startTransition] = React.useTransition();

  function go(orgId: string, kind: "checkout" | "portal") {
    startTransition(async () => {
      try {
        const { startCheckout, openBillingPortal } = await import("@/server/actions/billing");
        const res =
          kind === "checkout" ? await startCheckout(orgId) : await openBillingPortal(orgId);
        const url = "checkoutUrl" in res ? res.checkoutUrl : res.portalUrl;
        window.location.href = url;
      } catch (err) {
        toast.error("Couldn’t open billing", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return { pending, go };
}

/** CP-mode subscription state + Paddle checkout/portal actions. Renders the
 *  honest not-configured banner when Paddle isn't wired. */
function SubscriptionCard({ sub }: { sub: Subscription }) {
  const { pending, go } = useBillingPortal();

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
            {sub.billableUnits} billable {sub.billableUnits === 1 ? "unit" : "units"} ·{" "}
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
            <Button variant="outline" size="sm" onClick={() => go(sub.orgId, "portal")} disabled={pending}>
              Manage subscription
            </Button>
          ) : (
            <Button
              size="sm"
              onClick={() => go(sub.orgId, "checkout")}
              disabled={pending || sub.billableUnits < 1}
            >
              {sub.billableUnits < 1 ? "Within free tier" : "Subscribe"}
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
  cpBillingError,
}: {
  orgName: string;
  /** Null when the control plane could not be asked — see cpBillingError. */
  billing: Billing | null;
  servers: ServerItem[];
  /** CP-mode Paddle state; omitted in demo mode. */
  subscription?: Subscription;
  /** Why the control plane's billing figures are missing, when they are. */
  cpBillingError?: string | null;
}) {
  // Hoisted above the early return below: React requires every hook to run in
  // the same order on every render, and the SIGMA-242 unreachable-control-plane
  // branch returns before this point. Called conditionally, the hook's state
  // would shift slots the first time a failing control plane recovered — the
  // classic "rendered fewer hooks than expected" crash, on the page a customer
  // opens when they are already worried about their bill.
  const { pending: portalPending, go } = useBillingPortal();

  // The control plane did not answer (SIGMA-242). Everything below this point is
  // a monetary claim, and the only numbers we could put behind those claims come
  // from the local mirror, which describes a fleet and has never seen an
  // invoice or a subscription status. So: no figures at all, and say why.
  if (cpBillingError || !billing) {
    return (
      <div className="flex flex-col gap-6 p-4 md:p-6">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">Billing</h1>
        </div>
        <Alert variant="destructive">
          <AlertTriangle />
          <AlertTitle>Couldn’t reach the control plane</AlertTitle>
          <AlertDescription className="flex flex-col gap-2">
            <span>
              We can’t show {orgName}’s usage or subscription right now, so nothing on
              this page would be your bill. Your servers keep running — this is a
              reporting outage, not a billing change.
            </span>
            <span>
              If you were expecting a payment warning here, check your email or ask an
              operator: a past-due subscription would still be past due.
            </span>
            {cpBillingError && (
              <span className="font-mono text-xs opacity-80">{cpBillingError}</span>
            )}
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  const { unitPrice, freeTier, currency } = billing;
  const fc = (a: number, cents = false) => money(a, currency, cents);

  // Billing counts RUNNING servers only — the CP/Paddle charge basis (SIGMA-91).
  // gross (invoiceTotal) − free-tier credit == billing.amount (Total due), so the
  // preview stays internally consistent above the free tier.
  const connectedServers = servers.filter((s) => s.status === "running");
  const provisioningCount = servers.length - connectedServers.length;
  // Everything below counts UNITS, not servers: a GPU box is four of them, so a
  // three-server fleet can still be above the free tier.
  //
  // And it counts BILLED units, not live ones (SIGMA-292). The subscription is
  // priced on the high-water mark of the fleet over the last day, so an org that
  // shrank this morning is invoiced for what it ran, not for what is left. That
  // number used to exist only inside the drift sweep: the page showed the live
  // figure as "Total due", the customer approved it at checkout, and Paddle
  // charged the other one ten minutes later. Whatever we bill has to be on
  // screen, with the reason next to it.
  const billedUnits = billing.billedUnits ?? billing.units;
  const windowHours = billing.billingWindowHours ?? 0;
  const shrank = billedUnits > billing.units;
  const freeUsed = Math.min(billedUnits, freeTier);
  const freeRemaining = Math.max(0, freeTier - billedUnits);
  const billableCount = billing.billableUnits;
  const invoiceTotal = billedUnits * unitPrice;
  // An active subscription cannot go below one unit (Paddle rejects a
  // zero-quantity item), so a fleet that drops into the free tier still bills a
  // one-unit minimum. Saying "nothing due" while that line is invoiced is the
  // other half of SIGMA-292.
  const minimumUnits = Math.max(0, billableCount - Math.max(0, billedUnits - freeTier));

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">Billing</h1>
        <p className="text-sm text-muted-foreground">
          One simple meter for {orgName}: {fc(unitPrice)} per connected unit. An ordinary
          server is one unit; heavier servers to manage weigh more.
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
                {billedUnits} {billedUnits === 1 ? "unit" : "units"} × {fc(unitPrice)},{" "}
                {freeTier} free
              </p>
            )}
            <div className="rounded-lg bg-muted/60 px-3 py-2 text-xs text-muted-foreground">
              {billing.isFree ? (
                <>
                  Free while your fleet is {freeTier} units or fewer. An ordinary server
                  is 1 unit, a Kubernetes node 2, a GPU server 4.
                </>
              ) : minimumUnits > 0 ? (
                <>
                  Your fleet ({billedUnits} {billedUnits === 1 ? "unit" : "units"}) is inside
                  the {freeTier}-unit free tier, but an active subscription bills a{" "}
                  <span className="font-medium text-foreground">
                    {minimumUnits}-unit minimum
                  </span>{" "}
                  = {fc(billing.amount)} / month. Cancel it in the customer portal to stop
                  the charge.
                </>
              ) : (
                <>
                  ({billedUnits} {billedUnits === 1 ? "unit" : "units"} − {freeTier} free) ×{" "}
                  {fc(unitPrice)} ={" "}
                  <span className="font-medium text-foreground">{fc(billing.amount)}</span>{" "}
                  / month
                </>
              )}
            </div>
            {shrank && (
              <p className="text-xs text-muted-foreground">
                Billed on {billedUnits} {billedUnits === 1 ? "unit" : "units"} — the most your
                fleet ran in the last {windowHours} hours — not the {billing.units} connected
                right now. A scale-down takes effect a day later so a network blip can’t
                re-price your subscription twice in an hour; a scale-up is immediate.
              </p>
            )}
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
                  {freeUsed} <span className="text-muted-foreground">of</span> {freeTier}{" "}
                  <span className="text-base font-normal text-muted-foreground">units</span>
                </>
              ) : (
                <>
                  {billableCount} <span className="text-muted-foreground">of</span>{" "}
                  {billedUnits}{" "}
                  <span className="text-base font-normal text-muted-foreground">units billed</span>
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
                    ? `${freeRemaining} free ${freeRemaining === 1 ? "unit" : "units"} remaining`
                    : "Free tier full"
                  : `Above the ${freeTier}-unit free tier`}
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
              We count units, at a flat {fc(unitPrice)} each per month — an ordinary server
              is 1, a Kubernetes node 2, a GPU server 4. Every capability is included:
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

      {billing.breakdown.length > 0 && (
        <Card>
          <CardHeader className="border-b">
            <CardTitle className="text-base">How your fleet adds up</CardTitle>
            <CardDescription>
              Why {billing.connected} {billing.connected === 1 ? "server" : "servers"} bills as{" "}
              {billing.units} {billing.units === 1 ? "unit" : "units"}.
            </CardDescription>
          </CardHeader>
          <CardContent className="px-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4">Server type</TableHead>
                  <TableHead className="text-right">Connected</TableHead>
                  <TableHead className="text-right">Weight</TableHead>
                  <TableHead className="pr-4 text-right">Units</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {billing.breakdown.map((line) => (
                  <TableRow key={line.type}>
                    <TableCell className="pl-4 font-medium text-foreground">
                      {serverTypeLabel(line.type)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{line.count}</TableCell>
                    <TableCell className="text-right text-muted-foreground tabular-nums">
                      ×{line.weight}
                    </TableCell>
                    <TableCell className="pr-4 text-right tabular-nums">{line.units}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
              <TableFooter>
                <TableRow>
                  <TableCell colSpan={3} className="pl-4 font-medium text-foreground">
                    Total units
                  </TableCell>
                  <TableCell className="pr-4 text-right font-semibold tabular-nums">
                    {billing.units}
                  </TableCell>
                </TableRow>
              </TableFooter>
            </Table>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4 border-b">
          <div className="grid gap-1">
            <CardTitle>Invoice preview</CardTitle>
            <CardDescription>Current period · {currentPeriod()}</CardDescription>
          </div>
          {/* This used to be `toast.success("Invoice download started")` — a
              green confirmation, beside a real Paddle subscription card, for a
              request that was never made: there is no invoice endpoint in cp.ts
              or in the CP API. A paying customer fetching the month's invoice
              for their accountant got "Invoice download started · Acme · 1 Aug
              – 31 Aug", nothing downloaded, and opened a support ticket
              (SIGMA-239).

              Paddle is the merchant of record, so it holds the actual invoice —
              the portal is where the document lives, and this now goes there.
              With payments unconfigured there is no portal and no invoice, so
              the control is absent rather than dishonest. */}
          {subscription?.configured && (
            <Button
              variant="outline"
              size="sm"
              className="gap-1.5"
              onClick={() => go(subscription.orgId, "portal")}
              disabled={portalPending}
            >
              <Download className="size-3.5" />
              Download invoice
            </Button>
          )}
        </CardHeader>
        <CardContent className="px-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Connected server</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Region</TableHead>
                <TableHead className="text-right">Units</TableHead>
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
                  {connectedServers.length === 1 ? "server" : "servers"} ={" "}
                  {billedUnits} {billedUnits === 1 ? "unit" : "units"} × {fc(unitPrice)}
                  {shrank && (
                    <span className="text-muted-foreground/70">
                      {" "}
                      · peak of the last {windowHours}h, not the {billing.units} connected now
                    </span>
                  )}
                  {billing.isFree && (
                    <span className="text-muted-foreground/70"> · free tier applied</span>
                  )}
                </TableCell>
                <TableCell className="text-right text-muted-foreground tabular-nums">
                  {billedUnits}
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
              {minimumUnits > 0 && (
                <TableRow>
                  <TableCell colSpan={4} className="pl-4 text-muted-foreground">
                    Subscription minimum ({minimumUnits} × {fc(unitPrice)}) — an active
                    subscription cannot bill zero units; cancel it in the portal to stop
                    this line
                  </TableCell>
                  <TableCell className="pr-4 text-right text-muted-foreground tabular-nums">
                    {fc(minimumUnits * unitPrice, true)}
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
  const weight = serverUnitWeight(server.type);
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
          {serverTypeLabel(server.type)}
        </Badge>
      </TableCell>
      <TableCell className="text-muted-foreground">{server.region}</TableCell>
      <TableCell className="text-right text-muted-foreground tabular-nums">{weight}</TableCell>
      <TableCell className="pr-4 text-right tabular-nums">
        {money(weight * unitPrice, currency, true)}
      </TableCell>
    </TableRow>
  );
}
