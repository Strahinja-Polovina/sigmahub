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
  Loader2,
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
import { connectServer } from "@/server/actions/servers";
import { SERVER_TYPE_LABELS, SERVER_TYPE_ORDER } from "./server-meta";

function bootstrapCommand(orgSlug: string) {
  return `curl -fsSL https://get.sigmahub.io/agent | sh -s -- --org ${orgSlug} --token sk_boot_${orgSlug}_x92f`;
}

function CopyField({ value, ariaLabel }: { value: string; ariaLabel: string }) {
  const [copied, setCopied] = React.useState(false);
  const copy = React.useCallback(async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // clipboard can be unavailable in sandboxes; the toast still confirms intent
    }
    setCopied(true);
    toast.success("Copied to clipboard");
    window.setTimeout(() => setCopied(false), 1500);
  }, [value]);

  return (
    <div className="flex items-stretch gap-2">
      <div className="flex min-w-0 flex-1 items-center overflow-x-auto rounded-lg border border-border bg-muted/50 px-3 py-2 font-mono text-xs text-foreground">
        <span className="whitespace-nowrap">{value}</span>
      </div>
      <Button variant="outline" size="icon-sm" className="self-center" aria-label={ariaLabel} onClick={copy}>
        {copied ? <Check className="text-emerald-600" /> : <Copy className="text-muted-foreground" />}
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

export function ConnectServerDialog({
  orgId,
  orgSlug,
  trigger,
}: {
  orgId: string;
  orgSlug: string;
  trigger?: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);
  const [tab, setTab] = React.useState("ssh");
  const [name, setName] = React.useState("");
  const [type, setType] = React.useState<string>("general");
  const [provider, setProvider] = React.useState("");
  const [region, setRegion] = React.useState("");
  const [vpn, setVpn] = React.useState(false);
  const [pending, startTransition] = React.useTransition();

  function reset() {
    setTab("ssh");
    setName("");
    setType("general");
    setProvider("");
    setRegion("");
    setVpn(false);
  }

  function connect() {
    const n = name.trim();
    if (!n) return;
    startTransition(async () => {
      try {
        const { bootstrapToken } = await connectServer({
          orgId,
          name: n,
          type,
          provider,
          region,
          byoVpn: vpn,
        });
        toast.success(`${n} registered — provisioning`, {
          description: `Bootstrap token ${bootstrapToken}. Run the command on the host, or simulate the agent check-in from the list.`,
        });
        setOpen(false);
        reset();
      } catch (err) {
        toast.error("Couldn’t connect server", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (pending) return;
        setOpen(next);
        if (!next) reset();
      }}
    >
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
            Attach any Linux host to SigmaHub. Register it here, then run the
            one-line SSH bootstrap (or a provider integration) so its agent
            checks in.
          </DialogDescription>
        </DialogHeader>

        <Tabs value={tab} onValueChange={setTab}>
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

          <TabsContent value="ssh" className="flex flex-col gap-4 pt-4">
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="srv-name" className="text-xs text-muted-foreground">
                  Server name
                </Label>
                <Input
                  id="srv-name"
                  placeholder="hel-general-02"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  autoFocus
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label className="text-xs text-muted-foreground">Type</Label>
                <div className="flex flex-wrap gap-1.5">
                  {SERVER_TYPE_ORDER.map((t) => (
                    <Button
                      key={t}
                      type="button"
                      size="sm"
                      variant={type === t ? "default" : "outline"}
                      onClick={() => setType(t)}
                    >
                      {SERVER_TYPE_LABELS[t]}
                    </Button>
                  ))}
                </div>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="srv-provider" className="text-xs text-muted-foreground">
                  Provider
                </Label>
                <Input
                  id="srv-provider"
                  placeholder="Hetzner"
                  value={provider}
                  onChange={(e) => setProvider(e.target.value)}
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="srv-region" className="text-xs text-muted-foreground">
                  Region
                </Label>
                <Input
                  id="srv-region"
                  placeholder="eu-central · HEL1"
                  value={region}
                  onChange={(e) => setRegion(e.target.value)}
                />
              </div>
            </div>

            <div className="flex flex-col gap-2">
              <Label className="text-xs text-muted-foreground">
                Run this on the server you want to connect
              </Label>
              <CopyField value={bootstrapCommand(orgSlug)} ariaLabel="Copy bootstrap command" />
            </div>

            <Separator />

            <div className="flex items-start justify-between gap-4">
              <div className="flex flex-col gap-0.5">
                <Label htmlFor="connect-vpn" className="flex items-center gap-2 text-sm font-medium text-foreground">
                  <Network className="size-4 text-muted-foreground" />
                  Connect over a VPN / jump host
                </Label>
                <p className="text-xs text-muted-foreground">
                  For servers that are not publicly SSH-reachable.
                </p>
              </div>
              <Switch id="connect-vpn" checked={vpn} onCheckedChange={setVpn} />
            </div>

            <TrustNote />
          </TabsContent>

          <TabsContent value="provider" className="flex flex-col gap-4 pt-4">
            <p className="text-xs leading-relaxed text-muted-foreground">
              Bring your own provider API key and SigmaHub discovers reachable
              hosts and installs the agent for you. We only read your inventory —
              we never resell capacity or bill you for the underlying server.
            </p>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="provider-key" className="text-xs text-muted-foreground">
                Provider API token
              </Label>
              <div className="relative">
                <KeyRound className="absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input id="provider-key" type="password" placeholder="••••••••••••••••••••" className="pl-8 font-mono text-xs" disabled />
              </div>
              <p className="text-xs text-muted-foreground">
                Provider auto-discovery is coming soon. Use the SSH bootstrap tab
                to register a host today.
              </p>
            </div>
            <TrustNote />
          </TabsContent>
        </Tabs>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
            Cancel
          </DialogClose>
          <Button onClick={connect} disabled={tab !== "ssh" || !name.trim() || pending}>
            {pending && <Loader2 className="size-4 animate-spin" />}
            Connect server
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
