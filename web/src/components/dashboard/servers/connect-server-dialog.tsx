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
  ChevronDown,
  CircleAlert,
  Radio,
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
  agentCheckIn,
  changeServerType,
  connectServer,
  decommissionServer,
  provisionServer,
  serverConnectionState,
  type ServerConnectionState,
} from "@/server/actions/servers";
import {
  SERVER_CATALOG,
  SERVER_TYPE_LABELS,
  CONNECTABLE_SERVER_TYPES,
  serverTypeLabel,
} from "@/lib/server-catalog.generated";
import { SERVER_STATUS } from "@/lib/server-compat";

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

/** What the agent found, once it has checked in. This is the half of the
 *  connect form that used to be typed in by hand — hostname, OS, CPU, memory,
 *  disk, accelerator — and every value here is now read off the machine. */
function DetectedFacts({ state }: { state: ServerConnectionState }) {
  const rows: [string, string][] = [
    ["Hostname", state.name],
    ["OS", state.distro],
    ["Architecture", state.arch],
    ["CPU", state.cpu ? `${state.cpu} vCPU` : ""],
    ["Memory", state.memGb ? `${state.memGb} GB` : ""],
    ["Disk", state.diskTotalBytes ? `${Math.round(state.diskTotalBytes / 1_000_000_000)} GB` : ""],
    ["GPU", state.gpu],
  ];
  return (
    <dl className="grid grid-cols-2 gap-x-4 gap-y-1">
      {rows
        .filter(([, value]) => value)
        .map(([label, value]) => (
          <React.Fragment key={label}>
            <dt className="text-xs text-muted-foreground">{label}</dt>
            <dd className="truncate text-xs font-medium text-foreground">{value}</dd>
          </React.Fragment>
        ))}
    </dl>
  );
}

/** The gate's verdict and the two exits it implies (SIGMA-203).
 *
 *  The reasons are rendered VERBATIM — they are written by the side that made
 *  the decision, and a dashboard that re-worded them would eventually
 *  contradict the API in the one situation where the operator is already
 *  confused. */
export function IncompatiblePanel({
  orgId,
  state,
  onChanged,
  onDisconnected,
}: {
  orgId: string;
  state: ServerConnectionState;
  onChanged: (next: ServerConnectionState | null) => void;
  /** Called after the server has actually been disconnected, so the caller can
   *  close the dialog or navigate away. Omitted where disconnecting is already
   *  offered elsewhere on the page. */
  onDisconnected?: () => void;
}) {
  const [pending, startTransition] = React.useTransition();

  function retype(type: string) {
    startTransition(async () => {
      const res = await changeServerType({ orgId, serverId: state.id, type });
      if (!res.ok) {
        toast.error("Couldn’t change the server type", { description: res.error });
        return;
      }
      onChanged(res.state);
      if (res.state && res.state.status === SERVER_STATUS.incompatible) {
        toast.warning(`Still incompatible as a ${serverTypeLabel(type)} server`, {
          description: res.state.incompatibleReasons[0]?.reason,
        });
      } else {
        toast.success(`Connected as a ${serverTypeLabel(type)} server`);
      }
    });
  }

  // The graceful path (SIGMA-204), even here. This host was misfiled, not
  // broken: its agent is installed, authenticated and heartbeating, and the
  // operator ran our installer on it minutes ago. Tombstoning the row would
  // leave that installer's work — the binary, the unit, the tunnel — on a
  // machine we then claim to know nothing about.
  function disconnect() {
    startTransition(async () => {
      const res = await decommissionServer({ serverId: state.id });
      if (!res.ok) {
        toast.error("Couldn’t disconnect", { description: res.error });
        return;
      }
      toast.success(`Decommissioning ${state.name}…`, {
        description: "The agent removes what SigmaHub installed and then removes itself.",
      });
      onDisconnected?.();
    });
  }

  return (
    <Alert variant="destructive">
      <CircleAlert className="size-4" />
      <AlertTitle>This host isn’t a {serverTypeLabel(state.type)} server</AlertTitle>
      <AlertDescription>
        <ul className="flex flex-col gap-1">
          {state.incompatibleReasons.map((reason) => (
            <li key={reason.id}>{reason.reason}</li>
          ))}
        </ul>
        <p className="pt-2 text-xs">
          Nothing is scheduled onto it and it isn’t billed. Connect it as
          something it can be, or disconnect it.
        </p>
        <div className="flex flex-wrap items-center gap-1.5 pt-2">
          {CONNECTABLE_SERVER_TYPES.filter((t) => t !== state.type).map((t) => (
            <Button
              key={t}
              type="button"
              size="sm"
              variant="outline"
              disabled={pending}
              onClick={() => retype(t)}
            >
              {SERVER_TYPE_LABELS[t]}
            </Button>
          ))}
          <Button type="button" size="sm" variant="destructive" disabled={pending} onClick={disconnect}>
            Disconnect
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}

/** The live half of the connect step: the install command is already on screen,
 *  and this is what happens next. It polls the pre-created server row, so the
 *  operator watches "waiting for agent…" become the machine's own name — or the
 *  reason it was refused — without refreshing anything. */
function WaitingForAgent({
  orgId,
  serverId,
  cpMode,
  onDisconnected,
}: {
  orgId: string;
  serverId: string;
  cpMode?: boolean;
  onDisconnected?: () => void;
}) {
  const [state, setState] = React.useState<ServerConnectionState | null>(null);
  const [simulating, startSimulation] = React.useTransition();
  const settled = state ? state.status !== SERVER_STATUS.provisioning : false;

  React.useEffect(() => {
    if (settled) return;
    let cancelled = false;
    // 3s: fast enough that a real agent's registration (seconds after the
    // installer runs) shows up while the operator is still looking at the
    // dialog, slow enough not to hammer the control plane from an open tab.
    const tick = async () => {
      try {
        const next = await serverConnectionState({ orgId, serverId });
        if (!cancelled) setState(next);
      } catch {
        // A transient failure is not worth a toast while polling; the next
        // tick either recovers or the operator closes the dialog.
      }
    };
    void tick();
    const timer = window.setInterval(tick, 3000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [orgId, serverId, settled]);

  function simulate(shape: "matching" | "generic") {
    startSimulation(async () => {
      try {
        await agentCheckIn({ serverId, shape });
        setState(await serverConnectionState({ orgId, serverId }));
      } catch (err) {
        toast.error("Check-in failed", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  if (state?.status === SERVER_STATUS.incompatible) {
    return (
      <IncompatiblePanel
        orgId={orgId}
        state={state}
        onChanged={setState}
        onDisconnected={onDisconnected}
      />
    );
  }

  if (state && state.status !== SERVER_STATUS.provisioning) {
    return (
      <div className="flex flex-col gap-2 rounded-lg border border-border bg-muted/40 p-3">
        <p className="flex items-center gap-2 text-sm font-medium text-foreground">
          <Check className="size-4 text-emerald-600" />
          {state.name} checked in
        </p>
        <DetectedFacts state={state} />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-2 rounded-lg border border-border bg-muted/40 p-3">
      <p className="flex items-center gap-2 text-sm font-medium text-foreground">
        <Loader2 className="size-4 animate-spin text-muted-foreground" />
        Waiting for agent…
      </p>
      <p className="text-xs text-muted-foreground">
        Run the command above on the host. It appears here the moment the agent
        registers — with its hostname, OS, CPU, memory, disk and any GPU read
        off the machine.
      </p>
      {/* Demo mode has no real host to run anything on. Both shapes are
          offered because the second one — an ordinary box — is the only way to
          see the compatibility gate refuse a server without owning the wrong
          hardware. */}
      {!cpMode && (
        <div className="flex flex-wrap gap-1.5 pt-1">
          <Button type="button" size="sm" variant="outline" disabled={simulating} onClick={() => simulate("matching")}>
            {simulating ? <Loader2 className="size-3.5 animate-spin" /> : <Radio className="size-3.5" />}
            Simulate check-in
          </Button>
          <Button type="button" size="sm" variant="outline" disabled={simulating} onClick={() => simulate("generic")}>
            Simulate an ordinary box
          </Button>
        </div>
      )}
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
  // The two inputs the operator actually has answers to. Everything the
  // machine can report about itself — its name, its distro, its capacity — is
  // detected at registration instead of being typed here (SIGMA-202).
  const [ip, setIp] = React.useState("");
  const [type, setType] = React.useState<string>("general");
  // Undefined for a type the catalog has never heard of, so a stale value can
  // only cost the hint, never crash the dialog.
  const selectedType = SERVER_CATALOG[type as keyof typeof SERVER_CATALOG];
  const [showMeta, setShowMeta] = React.useState(false);
  const [provider, setProvider] = React.useState("");
  const [region, setRegion] = React.useState("");
  const [proxyRole, setProxyRole] = React.useState(false);
  // Default to KEEPING public SSH (SIGMA-179): mesh-only SSH is not yet a
  // working replacement — the mesh carries SigmaHub servers only, never an
  // operator device — so closing port 22 by default locks the operator out of
  // their own host with no in-product way back.
  const [keepPublicSsh, setKeepPublicSsh] = React.useState(true);
  const [vpn, setVpn] = React.useState(false);
  // The issued install command + the row it will fill in, shown together.
  const [issued, setIssued] = React.useState<{
    serverId: string;
    command: string;
    expiresAt?: string;
    bootstrapPubkey?: string;
  } | null>(null);
  const [pending, startTransition] = React.useTransition();

  function reset() {
    setIp("");
    setType("general");
    setShowMeta(false);
    setProvider("");
    setRegion("");
    setProxyRole(false);
    setKeepPublicSsh(true);
    setVpn(false);
    setIssued(null);
  }

  function connect() {
    const host = ip.trim();
    if (!host) return;
    startTransition(async () => {
      try {
        if (cpMode) {
          const res = await provisionServer({
            orgId,
            type,
            hostIp: host,
            provider,
            region,
            proxyRole,
            keepPublicSsh,
          });
          if (res.mode === "cp") {
            setIssued({
              serverId: res.serverId,
              command: res.command,
              expiresAt: res.expiresAt,
              bootstrapPubkey: res.bootstrapPubkey,
            });
          }
          return;
        }
        // Demo mode: the simulated one-liner path, on the same two-step shape
        // so the flow being demonstrated is the real one.
        const res = await connectServer({ orgId, type, hostIp: host, provider, region, byoVpn: vpn });
        if (res.mode === "sim") {
          setIssued({ serverId: res.id, command: bootstrapCommand(orgSlug) });
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
            Tell us where the host is and what you want it to be. The agent
            reports everything else — hostname, OS, CPU, memory, disk and GPU.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 pt-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="srv-ip" className="text-xs text-muted-foreground">
              Host IP or hostname
            </Label>
            <Input
              id="srv-ip"
              placeholder="203.0.113.7"
              value={ip}
              onChange={(e) => setIp(e.target.value)}
              disabled={Boolean(issued)}
              autoFocus
            />
          </div>

          <div className="flex flex-col gap-1.5">
            <Label className="text-xs text-muted-foreground">What is this server for?</Label>
            <div className="flex flex-wrap gap-1.5">
              {CONNECTABLE_SERVER_TYPES.map((t) => (
                <Button
                  key={t}
                  type="button"
                  size="sm"
                  variant={type === t ? "default" : "outline"}
                  disabled={Boolean(issued)}
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

          {/* Provider and region are labels for humans — nothing schedules,
              bills or validates on them — so they are optional and collapsed
              rather than sitting in the required path (SIGMA-202). */}
          {!issued && (
            <div className="flex flex-col gap-2">
              <button
                type="button"
                className="flex w-fit items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
                onClick={() => setShowMeta((v) => !v)}
              >
                <ChevronDown className={showMeta ? "size-3.5" : "size-3.5 -rotate-90"} />
                Provider and region (optional)
              </button>
              {showMeta && (
                <div className="grid gap-3 sm:grid-cols-2">
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
              )}
            </div>
          )}

          {cpMode && !issued && (
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
          {cpMode && !issued && (
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

          {/* The install command and the live state of the row it fills in,
              side by side: the token is minted the moment the operator presses
              Connect, so there is nothing to wait for before copying it. */}
          {issued ? (
            <div className="flex flex-col gap-2">
              <Label className="text-xs text-muted-foreground">
                Run this on the host — cosign-verified, the token is shown once
                {issued.expiresAt
                  ? ` and expires at ${new Date(issued.expiresAt).toLocaleTimeString()}`
                  : ""}
              </Label>
              <CopyField value={issued.command} ariaLabel="Copy install command" />
              {issued.bootstrapPubkey && (
                <>
                  <Label className="pt-1 text-xs text-muted-foreground">
                    Or, for hands-off SSH provisioning, authorize this one-time
                    key (the installer removes it afterward):
                  </Label>
                  <CopyField value={issued.bootstrapPubkey} ariaLabel="Copy bootstrap key" />
                </>
              )}
              <WaitingForAgent
                orgId={orgId}
                serverId={issued.serverId}
                cpMode={cpMode}
                onDisconnected={() => {
                  setOpen(false);
                  reset();
                }}
              />
            </div>
          ) : (
            <p className="text-xs text-muted-foreground">
              A cosign-verified install command is generated when you press
              “Connect server”.
            </p>
          )}

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
            <Switch id="connect-vpn" checked={vpn} onCheckedChange={setVpn} disabled={Boolean(issued)} />
          </div>

          <TrustNote />
        </div>

        <DialogFooter>
          {issued ? (
            // Once the install command is issued there is nothing left to
            // submit — a single Done closes the dialog instead of leaving a
            // permanently disabled "Connect server" next to it. The server
            // keeps registering (and the gate keeps running) either way; the
            // servers list shows how it ended up.
            <DialogClose render={<Button type="button" />}>Done</DialogClose>
          ) : (
            <>
              <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
                Cancel
              </DialogClose>
              <Button onClick={connect} disabled={!ip.trim() || pending}>
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
