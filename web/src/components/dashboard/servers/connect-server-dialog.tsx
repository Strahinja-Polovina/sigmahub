"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  Terminal,
  KeyRound,
  Check,
  Copy,
  ShieldCheck,
  Network,
  ArrowRight,
  Lock,
} from "lucide-react";

import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import { useActiveOrg } from "@/components/dashboard/org-context";

// One-line bootstrap. In the real product the token is org-scoped and single-use;
// here it is a stable mock derived from the org slug so the copy is deterministic.
function bootstrapCommand(orgSlug: string) {
  return `curl -fsSL https://get.sigmahub.io/agent | sh -s -- --org ${orgSlug} --token sk_boot_${orgSlug}_x92f`;
}

function CopyField({
  value,
  ariaLabel,
  mono = true,
}: {
  value: string;
  ariaLabel: string;
  mono?: boolean;
}) {
  const [copied, setCopied] = React.useState(false);

  const copy = React.useCallback(async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Clipboard can be unavailable in some sandboxes; the toast still confirms intent.
    }
    setCopied(true);
    toast.success("Copied to clipboard");
    window.setTimeout(() => setCopied(false), 1500);
  }, [value]);

  return (
    <div className="flex items-stretch gap-2">
      <div
        className={`flex min-w-0 flex-1 items-center overflow-x-auto rounded-lg border border-border bg-muted/50 px-3 py-2 ${
          mono ? "font-mono" : ""
        } text-xs text-foreground`}
      >
        <span className="whitespace-nowrap">{value}</span>
      </div>
      <Button
        variant="outline"
        size="icon-sm"
        className="self-center"
        aria-label={ariaLabel}
        onClick={copy}
      >
        {copied ? (
          <Check className="text-emerald-600" />
        ) : (
          <Copy className="text-muted-foreground" />
        )}
      </Button>
    </div>
  );
}

function TrustNote() {
  return (
    <div className="flex items-start gap-2.5 rounded-lg border border-border bg-muted/40 p-3">
      <ShieldCheck className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
      <p className="text-xs leading-relaxed text-muted-foreground">
        SigmaHub never resells or provisions servers — you bring your own. After
        bootstrap the agent dials home over WireGuard and stays{" "}
        <span className="font-medium text-foreground">outbound-only</span>: no
        inbound ports are opened and we never hold your SSH keys.
      </p>
    </div>
  );
}

function SshTab() {
  const { org } = useActiveOrg();
  const [vpn, setVpn] = React.useState(false);
  const command = bootstrapCommand(org.slug);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-2">
        <Label className="text-xs text-muted-foreground">
          Run this on the server you want to connect
        </Label>
        <CopyField value={command} ariaLabel="Copy bootstrap command" />
        <p className="text-xs text-muted-foreground">
          One line — installs the agent, registers it to {org.name}, and brings
          up the WireGuard tunnel. Re-runnable and idempotent.
        </p>
      </div>

      <Separator />

      <div className="flex flex-col gap-3">
        <div className="flex items-start justify-between gap-4">
          <div className="flex flex-col gap-0.5">
            <Label
              htmlFor="connect-vpn"
              className="flex items-center gap-2 text-sm font-medium text-foreground"
            >
              <Network className="size-4 text-muted-foreground" />
              Connect over a VPN / jump host
            </Label>
            <p className="text-xs text-muted-foreground">
              For servers that are not publicly SSH-reachable.
            </p>
          </div>
          <Switch
            id="connect-vpn"
            checked={vpn}
            onCheckedChange={setVpn}
          />
        </div>

        {vpn && (
          <div className="flex flex-col gap-3 rounded-lg border border-border bg-muted/30 p-3">
            <p className="text-xs leading-relaxed text-muted-foreground">
              Provide the reachable jump host or VPN endpoint. The bootstrap runs
              through it; the agent still ends up outbound-only over WireGuard.
            </p>
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="flex flex-col gap-1.5">
                <Label
                  htmlFor="vpn-endpoint"
                  className="text-xs text-muted-foreground"
                >
                  VPN / jump host endpoint
                </Label>
                <Input
                  id="vpn-endpoint"
                  placeholder="bastion.internal:22"
                  className="font-mono text-xs"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label
                  htmlFor="vpn-user"
                  className="text-xs text-muted-foreground"
                >
                  SSH user
                </Label>
                <Input
                  id="vpn-user"
                  placeholder="ops"
                  className="font-mono text-xs"
                />
              </div>
            </div>
          </div>
        )}
      </div>

      <TrustNote />
    </div>
  );
}

function ProviderTab() {
  const [provider, setProvider] = React.useState<"hetzner" | "ovh">("hetzner");

  return (
    <div className="flex flex-col gap-4">
      <p className="text-xs leading-relaxed text-muted-foreground">
        Bring your own provider API key and SigmaHub will discover reachable
        hosts and install the agent for you. We only read your inventory — we
        never resell capacity or bill you for the underlying server.
      </p>

      <div className="flex gap-2">
        {(["hetzner", "ovh"] as const).map((p) => (
          <Button
            key={p}
            type="button"
            variant={provider === p ? "default" : "outline"}
            size="sm"
            onClick={() => setProvider(p)}
          >
            {p === "hetzner" ? "Hetzner" : "OVH"}
          </Button>
        ))}
      </div>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="provider-key" className="text-xs text-muted-foreground">
          {provider === "hetzner" ? "Hetzner Cloud API token" : "OVH API key"}
        </Label>
        <div className="relative">
          <KeyRound className="absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            id="provider-key"
            type="password"
            placeholder="••••••••••••••••••••"
            className="pl-8 font-mono text-xs"
          />
        </div>
        <p className="text-xs text-muted-foreground">
          Stored encrypted, scoped to read + agent install. Revoke any time.
        </p>
      </div>

      <TrustNote />
    </div>
  );
}

export function ConnectServerDialog({
  trigger,
}: {
  trigger?: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          trigger ? (
            (trigger as React.ReactElement)
          ) : (
            <Button size="sm">
              <Terminal />
              Connect server
            </Button>
          )
        }
      />
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Connect a server</DialogTitle>
          <DialogDescription>
            Attach any Linux host to {""}
            SigmaHub. Two ways in — a one-line SSH bootstrap, or a provider
            integration.
          </DialogDescription>
        </DialogHeader>

        <Tabs defaultValue="ssh">
          <TabsList className="w-full">
            <TabsTrigger value="ssh" className="gap-1.5">
              <Terminal className="size-3.5" />
              SSH bootstrap
            </TabsTrigger>
            <TabsTrigger value="provider" className="gap-1.5">
              <Lock className="size-3.5" />
              Provider integration
            </TabsTrigger>
          </TabsList>

          <TabsContent value="ssh" className="pt-4">
            <SshTab />
          </TabsContent>
          <TabsContent value="provider" className="pt-4">
            <ProviderTab />
          </TabsContent>
        </Tabs>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
          <Button
            onClick={() => {
              toast.success("Waiting for the agent to check in…", {
                description: "This server will appear once it connects.",
              });
              setOpen(false);
            }}
          >
            Done
            <ArrowRight />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
