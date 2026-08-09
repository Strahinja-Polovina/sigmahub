"use client";

// The disconnect dialog (SIGMA-205).
//
// Disconnecting used to be a menu item that deleted a row and raised a toast
// saying "the agent tears down its WireGuard tunnel" — which was not true of
// anything. Nothing left the machine: not the binary, not the systemd unit, not
// the tunnel, not the containers, not the volumes.
//
// So this dialog does three things the menu item could not:
//
//   1. it states what will be removed, and what will NOT — named volumes are
//      the customer's data and stay unless they say otherwise;
//   2. it surfaces the control plane's 409 as the LIST of resources in the way,
//      rather than as an error string;
//   3. when the graceful path cannot work — an unreachable host, or a teardown
//      that timed out — it says so, offers the force path, and hands over the
//      script that finishes the job by hand.
//
// All of the deciding lives in @/lib/decommission; this file renders it.

import * as React from "react";
import { toast } from "sonner";
import { AlertTriangle, Check, Copy, Loader2, Trash2, Unplug } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import {
  boundResourcesMessage,
  forceReason,
  removalPlan,
  type ForceReason,
} from "@/lib/decommission";
import { MANUAL_UNINSTALL_SCRIPT } from "@/lib/uninstall-script";
import { decommissionServer, forceDisconnectServer } from "@/server/actions/servers";

function ScriptBlock({ value }: { value: string }) {
  const [copied, setCopied] = React.useState(false);
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between">
        <Label className="text-xs text-muted-foreground">
          Run this on the host to remove what is left behind
        </Label>
        <Button
          variant="outline"
          size="sm"
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(value);
            } catch {
              // clipboard can be unavailable in sandboxes; the toast still
              // confirms intent and the script is selectable below.
            }
            setCopied(true);
            toast.success("Cleanup script copied");
            window.setTimeout(() => setCopied(false), 1500);
          }}
        >
          {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
          Copy script
        </Button>
      </div>
      <pre className="max-h-56 overflow-auto rounded-lg border border-border bg-muted/50 p-3 font-mono text-[11px] leading-relaxed text-foreground">
        {value}
      </pre>
    </div>
  );
}

export function DisconnectServerDialog({
  open,
  onOpenChange,
  serverId,
  serverName,
  status,
  decommissioningSince,
  lastSeenAt,
  onDisconnected,
}: {
  open: boolean;
  onOpenChange: (next: boolean) => void;
  serverId: string;
  serverName: string;
  status: string;
  decommissioningSince?: Date | string | null;
  lastSeenAt?: Date | string | null;
  /** Called once the server is gone or on its way out, so the caller can
   *  navigate away from a page that is about to describe nothing. */
  onDisconnected?: () => void;
}) {
  const [purgeVolumes, setPurgeVolumes] = React.useState(false);
  const [pending, startTransition] = React.useTransition();
  const [bound, setBound] = React.useState<string[]>([]);
  const [error, setError] = React.useState<string | null>(null);

  // Which of the two disconnects this dialog is offering. DERIVED, not stored:
  // the answer is a function of the server's current state, and a copy in state
  // would keep showing the graceful path for a machine that has since gone
  // quiet — the one case where pressing it does nothing at all.
  const force: ForceReason = forceReason({ status, decommissioningSince, lastSeenAt });
  const plan = removalPlan(purgeVolumes);

  // Reset on CLOSE rather than on open: the operator who dismissed a
  // bound-resources refusal, went and deleted those resources, and came back
  // should not be greeted by the stale complaint.
  function handleOpenChange(next: boolean) {
    if (!next) {
      setBound([]);
      setError(null);
      setPurgeVolumes(false);
    }
    onOpenChange(next);
  }

  function run(action: () => Promise<{ ok: boolean; error?: string; boundResources?: string[] }>, done: string) {
    startTransition(async () => {
      const res = await action();
      if (!res.ok) {
        setBound(res.boundResources ?? []);
        // A 409 is answered with the names; anything else keeps its own text.
        setError(res.boundResources?.length ? null : (res.error ?? "Please try again."));
        return;
      }
      toast.success(done);
      handleOpenChange(false);
      onDisconnected?.();
    });
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Disconnect {serverName}</DialogTitle>
          <DialogDescription>
            {force
              ? force.message
              : "The agent removes everything SigmaHub put on this host, reports back, and then removes itself. The server stays listed as Decommissioning until it confirms."}
          </DialogDescription>
        </DialogHeader>

        {/* The 409, as a list. The control plane refuses while resources are
            still bound; naming them is what makes the refusal actionable. */}
        {bound.length > 0 && (
          <div
            role="alert"
            className="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive"
          >
            <p className="flex items-center gap-2 font-medium">
              <AlertTriangle className="size-4" />
              Nothing was disconnected
            </p>
            <p className="mt-1 text-destructive/90">{boundResourcesMessage(bound)}</p>
            <ul className="mt-2 flex flex-wrap gap-1.5">
              {bound.map((name) => (
                <li
                  key={name}
                  className="rounded-md border border-destructive/30 bg-card px-1.5 py-0.5 font-mono text-xs"
                >
                  {name}
                </li>
              ))}
            </ul>
          </div>
        )}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}

        {!force && (
          <>
            <ul className="flex flex-col gap-2.5">
              {plan.map((item) => (
                <li key={item.label} className="flex gap-2.5 text-sm">
                  <span
                    aria-hidden
                    className={
                      item.destructive
                        ? "mt-1.5 size-1.5 shrink-0 rounded-full bg-destructive"
                        : "mt-1.5 size-1.5 shrink-0 rounded-full bg-emerald-500"
                    }
                  />
                  <span className="flex flex-col">
                    <span className="font-medium text-foreground">{item.label}</span>
                    <span className="text-xs text-muted-foreground">{item.detail}</span>
                  </span>
                </li>
              ))}
            </ul>

            {/* Default OFF, and a separate decision from disconnecting. */}
            <Label className="flex items-start gap-2.5 rounded-lg border border-border p-3">
              <Checkbox
                checked={purgeVolumes}
                onCheckedChange={(next) => setPurgeVolumes(next === true)}
                aria-label="Also delete application data (volumes)"
              />
              <span className="flex flex-col gap-0.5">
                <span className="text-sm font-medium text-foreground">
                  Also delete application data (volumes)
                </span>
                <span className="text-xs font-normal text-muted-foreground">
                  Permanently deletes database data directories and uploaded files on this host.
                  Leave this off to keep them — they survive the machine.
                </span>
              </span>
            </Label>
          </>
        )}

        {/* The force path always carries the script: after it runs, everything
            SigmaHub installed is still on that box and we can no longer reach
            it to say so. */}
        {force && <ScriptBlock value={MANUAL_UNINSTALL_SCRIPT} />}

        <DialogFooter className="gap-2 sm:justify-between">
          <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
            Cancel
          </DialogClose>
          {force ? (
            <Button
              variant="destructive"
              disabled={pending}
              onClick={() =>
                run(
                  () => forceDisconnectServer({ serverId }),
                  `${serverName} removed. Run the cleanup script on the host.`
                )
              }
            >
              {pending ? <Loader2 className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
              Force disconnect
            </Button>
          ) : (
            <Button
              variant="destructive"
              disabled={pending}
              onClick={() =>
                run(
                  () => decommissionServer({ serverId, purgeVolumes }),
                  `Decommissioning ${serverName}…`
                )
              }
            >
              {pending ? <Loader2 className="size-4 animate-spin" /> : <Unplug className="size-4" />}
              {purgeVolumes ? "Disconnect and delete data" : "Disconnect"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
