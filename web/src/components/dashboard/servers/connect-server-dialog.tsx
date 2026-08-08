"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  Terminal,
  Check,
  Copy,
  ShieldCheck,
  ShieldAlert,
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
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { connectServer, provisionServer } from "@/server/actions/servers";
import {
  SERVER_CATALOG,
  SERVER_TYPE_LABELS,
  CONNECTABLE_SERVER_TYPES,
  SUPPORTED_DISTROS,
} from "@/lib/server-catalog.generated";

// The distro list comes from the catalog, not from here. It used to be a third
// hand-written copy, and it had already drifted: this dropdown said "Ubuntu
// 24.04 LTS" while the requirement checklist rendered three lines below it —
// in this same component — said "Ubuntu 24.04", for the same distro. A list the
// control plane rejects on is not a list the dialog gets to invent.
const DISTROS = SUPPORTED_DISTROS;

function bootstrapCommand(orgSlug: string) {
  // The real one-time token is minted server-side on connect; never render a
  // literal secret (even a fake one) into the client bundle.
  return `curl -fsSL https://get.sigmahub.io/agent | sh -s -- --org ${orgSlug} --token <one-time-token>`;
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
  cpMode,
  trigger,
}: {
  orgId: string;
  orgSlug: string;
  cpMode?: boolean;
  trigger?: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");
  const [ip, setIp] = React.useState("");
  const [type, setType] = React.useState<string>("general");
  // Undefined for a type the catalog has never heard of, so a stale value can
  // only cost the hint, never crash the dialog.
  const selectedType = SERVER_CATALOG[type as keyof typeof SERVER_CATALOG];
  const [provider, setProvider] = React.useState("");
  const [region, setRegion] = React.useState("");
  const [distro, setDistro] = React.useState("ubuntu-24.04");
  const [proxyRole, setProxyRole] = React.useState(false);
  // Default to KEEPING public SSH (SIGMA-179): mesh-only SSH is not yet a
  // working replacement — the mesh carries SigmaHub servers only, never an
  // operator device — so closing port 22 by default locks the operator out of
  // their own host with no in-product way back.
  const [keepPublicSsh, setKeepPublicSsh] = React.useState(true);
  const [vpn, setVpn] = React.useState(false);
  // CP mode: the real one-time install command + bootstrap key, shown once.
  const [issued, setIssued] = React.useState<{ command: string; expiresAt: string; bootstrapPubkey?: string } | null>(null);
  const [pending, startTransition] = React.useTransition();

  function reset() {
    setName("");
    setIp("");
    setType("general");
    setProvider("");
    setRegion("");
    setDistro("ubuntu-24.04");
    setProxyRole(false);
    setKeepPublicSsh(true);
    setVpn(false);
    setIssued(null);
  }

  function connect() {
    const n = name.trim();
    if (!n) return;
    startTransition(async () => {
      try {
        if (cpMode) {
          // SSH onboarding wizard: pre-create the server + mint the bootstrap
          // keypair; the operator runs the returned cosign-verified installer.
          const res = await provisionServer({
            orgId,
            name: n,
            type,
            provider,
            region,
            proxyRole,
            distro,
            keepPublicSsh,
            hostIp: ip,
          });
          if (res.mode === "cp") {
            setIssued({ command: res.command, expiresAt: res.expiresAt, bootstrapPubkey: res.bootstrapPubkey });
            toast.success(`Provisioned ${n}`, {
              description: "Run the installer on the host; it joins the mesh and appears as Ready.",
            });
          }
          return;
        }
        // Demo mode: the simulated one-liner path.
        const res = await connectServer({ orgId, name: n, type, provider, region, byoVpn: vpn });
        if (res.mode === "sim") {
          toast.success(`${n} registered — provisioning`, {
            description: `Run the command on the host, or simulate the agent check-in from the list.`,
          });
          setOpen(false);
          reset();
        }
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
            one-line SSH bootstrap so its agent checks in.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 pt-2">
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
                  {CONNECTABLE_SERVER_TYPES.map((t) => (
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
                {/* What the type means and what the host has to actually have.
                    Both come from the control plane's catalog, so the operator
                    reads the same requirements the registration gate will apply
                    — before spending ten minutes on an install, not after. */}
                {selectedType && (
                  <div className="flex flex-col gap-1 pt-0.5">
                    <p className="text-xs text-muted-foreground">{selectedType.hint}</p>
                    <ul className="flex flex-col gap-0.5">
                      {selectedType.requires.checks.map((c) => (
                        <li key={c.id} className="text-xs text-muted-foreground">
                          · {c.text}
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
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
              {cpMode && (
                <>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="srv-ip" className="text-xs text-muted-foreground">
                      Host IP / address
                    </Label>
                    <Input
                      id="srv-ip"
                      placeholder="203.0.113.7"
                      value={ip}
                      onChange={(e) => setIp(e.target.value)}
                    />
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="srv-distro" className="text-xs text-muted-foreground">
                      OS
                    </Label>
                    <Select value={distro} onValueChange={(v) => setDistro(v ?? "ubuntu-24.04")}>
                      <SelectTrigger id="srv-distro">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {DISTROS.map((d) => (
                          <SelectItem key={d.id} value={d.id}>
                            {d.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                </>
              )}
            </div>

            {cpMode && (
              <div className="flex items-start justify-between gap-4">
                <div className="flex flex-col gap-0.5">
                  <Label htmlFor="srv-proxy" className="flex items-center gap-2 text-sm font-medium text-foreground">
                    <Network className="size-4 text-muted-foreground" />
                    Proxy / edge role
                  </Label>
                  <p className="text-xs text-muted-foreground">
                    Opens 80/443 in the firewall for the ingress proxy (P1-8).
                  </p>
                </div>
                <Switch id="srv-proxy" checked={proxyRole} onCheckedChange={setProxyRole} />
              </div>
            )}

            {/* Keeping public SSH is the DEFAULT: closing port 22 is only safe
                when the operator has another way in, and the mesh is not one
                today (it carries SigmaHub servers, never an operator device),
                so the lockdown is opt-in and warned (SIGMA-179). */}
            {cpMode && (
              <div className="flex flex-col gap-2">
                <div className="flex items-start justify-between gap-4">
                  <div className="flex flex-col gap-0.5">
                    <Label htmlFor="keep-ssh" className="flex items-center gap-2 text-sm font-medium text-foreground">
                      <Lock className="size-4 text-muted-foreground" />
                      Keep public SSH open
                    </Label>
                    <p className="text-xs text-muted-foreground">
                      On (default): port 22 stays reachable, with password auth
                      and root login disabled. Turn off only if you have another
                      way into this host.
                    </p>
                  </div>
                  <Switch id="keep-ssh" checked={keepPublicSsh} onCheckedChange={setKeepPublicSsh} />
                </div>
                {!keepPublicSsh && (
                  <Alert variant="destructive">
                    <ShieldAlert className="size-4" />
                    <AlertTitle>You may lose access to this host</AlertTitle>
                    <AlertDescription>
                      Port 22 will be firewalled off after enrollment. SigmaHub
                      does not yet put your workstation on the mesh, so unless you
                      keep a bastion, VPN or provider console, this host becomes
                      unreachable over SSH.
                    </AlertDescription>
                  </Alert>
                )}
              </div>
            )}

            <div className="flex flex-col gap-2">
              {cpMode ? (
                issued ? (
                  <>
                    <Label className="text-xs text-muted-foreground">
                      Run this on the host — cosign-verified, the token is shown
                      once and expires at {new Date(issued.expiresAt).toLocaleTimeString()}
                    </Label>
                    <CopyField value={issued.command} ariaLabel="Copy install command" />
                    {issued.bootstrapPubkey && (
                      <>
                        <Label className="pt-1 text-xs text-muted-foreground">
                          Or, for hands-off SSH provisioning, authorize this
                          one-time key (the installer removes it afterward):
                        </Label>
                        <CopyField value={issued.bootstrapPubkey} ariaLabel="Copy bootstrap key" />
                      </>
                    )}
                  </>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    A cosign-verified install command is generated when you press
                    “Connect server”.
                  </p>
                )
              ) : (
                <>
                  <Label className="text-xs text-muted-foreground">
                    Run this on the server you want to connect
                  </Label>
                  <CopyField value={bootstrapCommand(orgSlug)} ariaLabel="Copy bootstrap command" />
                </>
              )}
            </div>

            <Separator />

            <div className="flex items-start justify-between gap-4">
              <div className="flex flex-col gap-0.5">
                <Label htmlFor="connect-vpn" className="flex items-center gap-2 text-sm font-medium text-foreground">
                  <Network className="size-4 text-muted-foreground" />
                  Connect over a VPN / jump host
                </Label>
                <p className="text-xs text-muted-foreground">
                  For servers that are not publicly SSH-reachable (the same
                  one-liner runs over your bastion).
                </p>
              </div>
              <Switch id="connect-vpn" checked={vpn} onCheckedChange={setVpn} />
            </div>

            <TrustNote />
        </div>

        <DialogFooter>
          {issued ? (
            // Once the install command is issued there is nothing left to
            // submit — a single Done closes the dialog instead of leaving a
            // permanently disabled "Connect server" next to it.
            <DialogClose render={<Button type="button" />}>Done</DialogClose>
          ) : (
            <>
              <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
                Cancel
              </DialogClose>
              <Button onClick={connect} disabled={!name.trim() || pending}>
                {pending && <Loader2 className="size-4 animate-spin" />}
                Connect server
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
