"use client";

import * as React from "react";
import { toast } from "sonner";
import { BellRing, Loader2, Plus, Send, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  createAlertChannel,
  deleteAlertChannel,
  listAlertChannels,
  setAlertRules,
  testAlertChannel,
} from "@/server/actions/alerts";
import type { CpAlertChannel } from "@/server/cp";

const EVENT_LABELS: Record<string, string> = {
  server_unreachable: "Server unreachable",
  server_recovered: "Server recovered",
  // SIGMA-233. Worth its own words rather than the raw event key: this is the
  // disconnect that did NOT finish, so the host is still running everything we
  // installed on it and somebody has to go and run the cleanup script.
  decommission_timed_out: "Decommission timed out (host not cleaned up)",
  deploy_failed: "Deploy failed",
  backup_failed: "Backup failed",
  verify_failed: "Restore-verify failed",
  cert_failed: "Certificate issuance failed",
  cert_expiring: "Certificate expiring",
};

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

// AlertsTab wires org-wide notification channels (P2-6): where operational
// events land outside the dashboard. Mutations are Org Admin; delivery health
// is shown honestly per channel (last success / last transport error).
export function AlertsTab({ orgId, isAdmin }: { orgId: string; isAdmin: boolean }) {
  const [channels, setChannels] = React.useState<CpAlertChannel[] | null>(null);
  const [events, setEvents] = React.useState<string[]>([]);
  const [pending, startTransition] = React.useTransition();

  const load = React.useCallback(() => {
    startTransition(async () => {
      try {
        const res = await listAlertChannels(orgId);
        setChannels(res.channels);
        setEvents(res.events);
      } catch (err) {
        toast.error("Couldn’t load alert channels", { description: errMsg(err) });
        setChannels([]);
      }
    });
  }, [orgId]);

  React.useEffect(() => {
    load();
  }, [load]);

  function toggleEvent(ch: CpAlertChannel, event: string) {
    if (!isAdmin) return;
    const next = ch.events.includes(event)
      ? ch.events.filter((e) => e !== event)
      : [...ch.events, event];
    startTransition(async () => {
      try {
        await setAlertRules({ orgId, channelId: ch.id, events: next });
        setChannels((prev) =>
          prev?.map((c) => (c.id === ch.id ? { ...c, events: next } : c)) ?? prev
        );
      } catch (err) {
        toast.error("Couldn’t update rules", { description: errMsg(err) });
      }
    });
  }

  function fireTest(ch: CpAlertChannel) {
    startTransition(async () => {
      try {
        await testAlertChannel({ orgId, channelId: ch.id });
        toast.success(`Test sent to ${ch.name}`);
        load();
      } catch (err) {
        toast.error("Test delivery failed", { description: errMsg(err) });
      }
    });
  }

  function remove(ch: CpAlertChannel) {
    startTransition(async () => {
      try {
        await deleteAlertChannel({ orgId, channelId: ch.id, name: ch.name });
        toast.success(`Removed ${ch.name}`);
        load();
      } catch (err) {
        toast.error("Couldn’t remove channel", { description: errMsg(err) });
      }
    });
  }

  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle className="flex items-center gap-2 text-sm">
            <BellRing className="size-4 text-muted-foreground" />
            Alert channels
          </CardTitle>
          <CardDescription>
            Operational events — unreachable servers, failed deploys, failed backups, certificate
            problems — delivered to Slack, Telegram, email or a signed webhook.
          </CardDescription>
        </div>
        {isAdmin && <AddChannelDialog orgId={orgId} onCreated={load} />}
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {channels === null ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : channels.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No channels yet. {isAdmin ? "Add one to get notified when something breaks." : "An org admin can add one."}
          </p>
        ) : (
          channels.map((ch) => (
            <div key={ch.id} className="rounded-lg border border-border">
              <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border px-4 py-2.5">
                <span className="inline-flex items-center gap-2 text-sm font-medium text-foreground">
                  {ch.name}
                  <span className="rounded-full border border-border px-2 py-0.5 text-xs text-muted-foreground">
                    {ch.kind}
                  </span>
                  {ch.lastError ? (
                    <span
                      className="rounded-full border border-destructive/40 px-2 py-0.5 text-xs text-destructive"
                      title={ch.lastError}
                    >
                      delivery failing
                    </span>
                  ) : ch.lastOkAt ? (
                    <span className="rounded-full border border-emerald-500/30 px-2 py-0.5 text-xs text-emerald-700">
                      delivering
                    </span>
                  ) : (
                    <span className="rounded-full border border-border px-2 py-0.5 text-xs text-muted-foreground">
                      never delivered
                    </span>
                  )}
                </span>
                {isAdmin && (
                  <span className="flex items-center gap-1">
                    <Button variant="ghost" size="sm" onClick={() => fireTest(ch)} disabled={pending}>
                      <Send className="size-3.5" />
                      Test
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-muted-foreground hover:text-destructive"
                      onClick={() => remove(ch)}
                      disabled={pending}
                    >
                      <Trash2 className="size-3.5" />
                    </Button>
                  </span>
                )}
              </div>
              <div className="flex flex-wrap gap-1.5 px-4 py-3">
                {events.map((ev) => {
                  const on = ch.events.includes(ev);
                  return (
                    <button
                      key={ev}
                      type="button"
                      disabled={!isAdmin || pending}
                      onClick={() => toggleEvent(ch, ev)}
                      className={`rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors ${
                        on
                          ? "border-primary/40 bg-primary/10 text-primary"
                          : "border-border text-muted-foreground"
                      } ${isAdmin ? "cursor-pointer hover:border-primary/40" : "cursor-default"}`}
                      title={on ? "Click to mute" : "Click to enable"}
                    >
                      {EVENT_LABELS[ev] ?? ev}
                    </button>
                  );
                })}
              </div>
              {ch.lastError && (
                <p className="border-t border-border px-4 py-2 text-xs text-destructive">
                  Last delivery error: {ch.lastError}
                </p>
              )}
            </div>
          ))
        )}
      </CardContent>
    </Card>
  );
}

function AddChannelDialog({ orgId, onCreated }: { orgId: string; onCreated: () => void }) {
  const [open, setOpen] = React.useState(false);
  const [kind, setKind] = React.useState("slack");
  const [name, setName] = React.useState("");
  const [secret, setSecret] = React.useState("");
  const [chatId, setChatId] = React.useState("");
  const [url, setUrl] = React.useState("");
  const [host, setHost] = React.useState("");
  const [port, setPort] = React.useState("587");
  const [from, setFrom] = React.useState("");
  const [to, setTo] = React.useState("");
  const [username, setUsername] = React.useState("");
  const [pending, startTransition] = React.useTransition();

  function reset() {
    setName("");
    setSecret("");
    setChatId("");
    setUrl("");
    setHost("");
    setPort("587");
    setFrom("");
    setTo("");
    setUsername("");
  }

  function submit() {
    const config: Record<string, unknown> =
      kind === "telegram"
        ? { chatId: chatId.trim() }
        : kind === "webhook"
          ? { url: url.trim() }
          : kind === "email"
            ? {
                host: host.trim(),
                port: Number(port) || 587,
                from: from.trim(),
                to: to.split(",").map((v) => v.trim()).filter(Boolean),
                username: username.trim(),
              }
            : {};
    startTransition(async () => {
      try {
        await createAlertChannel({
          orgId,
          kind,
          name: name.trim(),
          config,
          secret: secret.trim() || undefined,
        });
        toast.success(`Added ${name.trim()}`);
        setOpen(false);
        reset();
        onCreated();
      } catch (err) {
        toast.error("Couldn’t add channel", { description: errMsg(err) });
      }
    });
  }

  const secretLabel =
    kind === "slack"
      ? "Slack webhook URL"
      : kind === "telegram"
        ? "Bot token"
        : kind === "webhook"
          ? "Signing key (optional)"
          : "SMTP password (optional)";

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
          <Button size="sm" className="gap-1.5">
            <Plus className="size-3.5" />
            Add channel
          </Button>
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add an alert channel</DialogTitle>
          <DialogDescription>
            Credentials are stored encrypted (per-org envelope) and never shown again. All events
            start enabled — mute individual ones afterwards.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4">
          <div className="grid grid-cols-2 gap-3">
            <div className="flex flex-col gap-2">
              <Label>Type</Label>
              <Select value={kind} onValueChange={(v) => setKind(v ?? "slack")}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="slack">Slack</SelectItem>
                  <SelectItem value="telegram">Telegram</SelectItem>
                  <SelectItem value="webhook">Webhook</SelectItem>
                  <SelectItem value="email">Email (SMTP)</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="al-name">Name</Label>
              <Input
                id="al-name"
                placeholder="ops-alerts"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
          </div>

          {kind === "telegram" && (
            <div className="flex flex-col gap-2">
              <Label htmlFor="al-chat">Chat ID</Label>
              <Input
                id="al-chat"
                placeholder="-1001234567890"
                value={chatId}
                onChange={(e) => setChatId(e.target.value)}
              />
            </div>
          )}
          {kind === "webhook" && (
            <div className="flex flex-col gap-2">
              <Label htmlFor="al-url">URL</Label>
              <Input
                id="al-url"
                placeholder="https://example.com/hooks/sigmahub"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                Payloads are signed with X-Sigmahub-Signature-256 when a signing key is set.
              </p>
            </div>
          )}
          {kind === "email" && (
            <>
              <div className="grid grid-cols-3 gap-3">
                <div className="col-span-2 flex flex-col gap-2">
                  <Label htmlFor="al-host">SMTP host</Label>
                  <Input id="al-host" placeholder="smtp.example.com" value={host} onChange={(e) => setHost(e.target.value)} />
                </div>
                <div className="flex flex-col gap-2">
                  <Label htmlFor="al-port">Port</Label>
                  <Input id="al-port" placeholder="587" value={port} onChange={(e) => setPort(e.target.value)} />
                </div>
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div className="flex flex-col gap-2">
                  <Label htmlFor="al-from">From</Label>
                  <Input id="al-from" placeholder="alerts@example.com" value={from} onChange={(e) => setFrom(e.target.value)} />
                </div>
                <div className="flex flex-col gap-2">
                  <Label htmlFor="al-to">To (comma-separated)</Label>
                  <Input id="al-to" placeholder="ops@example.com" value={to} onChange={(e) => setTo(e.target.value)} />
                </div>
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="al-user">SMTP username (optional)</Label>
                <Input id="al-user" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
              </div>
            </>
          )}

          <div className="flex flex-col gap-2">
            <Label htmlFor="al-secret">{secretLabel}</Label>
            <Input
              id="al-secret"
              type="password"
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
              autoComplete="off"
            />
          </div>
        </div>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
            Cancel
          </DialogClose>
          <Button onClick={submit} disabled={pending || !name.trim()}>
            {pending && <Loader2 className="size-4 animate-spin" />}
            Add channel
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
