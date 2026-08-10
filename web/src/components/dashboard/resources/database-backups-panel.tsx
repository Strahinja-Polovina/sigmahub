"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import {
  Archive,
  CheckCircle2,
  Clock,
  History,
  Loader2,
  Plus,
  RefreshCcw,
  ShieldAlert,
  XCircle,
} from "lucide-react";
import { toast } from "sonner";

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
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  createBackupTarget,
  listBackupRuns,
  restoreDatabase,
  restoreDatabaseToTimestamp,
  setBackupPolicy,
} from "@/server/actions/backups";
import type { CpBackupPolicy, CpBackupRun, CpBackupTarget } from "@/server/cp";

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

/** A server a restore may be provisioned onto. Structurally the org server list
 *  the resource page already loads; `status` is what makes a target refusable. */
export type RestoreTargetServer = {
  id: string;
  name: string;
  type?: string;
  status?: string;
};

/** Server states in which nothing will ever act on a queued restore, and the
 *  half-sentence that explains each one to the operator. */
const DEAD_TARGET_REASON: Record<string, string> = {
  unreachable: "is unreachable — the control plane stopped hearing from its agent, so nothing there will pick the restore up",
  decommissioning: "is being decommissioned — its agent is tearing itself down and will not run new work",
};

/**
 * Decide what "Queue restore" should do, given the server the operator picked.
 *
 * SIGMA-241: both restore dialogs used to take the SOURCE resource's serverId
 * and pass it straight through, with no way to say otherwise. The one scenario
 * a restore-into-a-new-database exists for is a host that died — and in that
 * scenario the source's server is exactly the machine that cannot run the
 * restore. The control plane accepted the request, provisioned (and billed) a
 * fresh database, wrote the op into a desired-state document nobody was
 * polling, and the operator watched a restore that had never started fail to
 * finish, with no error because nothing had failed.
 *
 * So: the dialogs pick a target, and a target that cannot act is refused HERE,
 * before a resource is provisioned, with a reason that names the host. An id
 * the org server list does not contain is deferred to the control plane rather
 * than refused — the list is a page-load read that can fail, and a stale local
 * list must not be the thing that blocks a restore.
 */
export function planRestoreTarget(
  targetServerId: string,
  servers: RestoreTargetServer[]
): { ok: true; serverId: string } | { ok: false; reason: string } {
  const id = targetServerId.trim();
  if (!id) {
    return { ok: false, reason: "Choose the server to restore onto." };
  }
  const target = servers.find((s) => s.id === id);
  if (!target) return { ok: true, serverId: id };
  const why = target.status ? DEAD_TARGET_REASON[target.status] : undefined;
  if (why) {
    return {
      ok: false,
      reason: `${target.name} ${why}. Pick a server that is running — the backups themselves are fine.`,
    };
  }
  return { ok: true, serverId: id };
}

/** The target picker shared by both restore dialogs. */
function RestoreTargetField({
  id,
  servers,
  value,
  onChange,
  sourceServerId,
}: {
  id: string;
  servers: RestoreTargetServer[];
  value: string;
  onChange: (v: string) => void;
  sourceServerId: string | null;
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>Restore onto</Label>
      <Select value={value} onValueChange={(v) => onChange(v ?? "")}>
        <SelectTrigger id={id} className="w-full">
          <SelectValue placeholder="Choose a server…" />
        </SelectTrigger>
        <SelectContent>
          {servers.map((sv) => (
            <SelectItem key={sv.id} value={sv.id}>
              {sv.name}
              {sv.id === sourceServerId ? " · current host" : ""}
              {sv.status && sv.status !== "running" ? ` · ${sv.status}` : ""}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <p className="text-xs text-muted-foreground">
        Defaults to the source database’s host. When that host is gone, aim the
        restore at its replacement — the snapshots are in your bucket, not on the
        dead machine.
      </p>
    </div>
  );
}

function RunStatus({ status }: { status: string }) {
  switch (status) {
    case "success":
      return (
        <span className="inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-400">
          <CheckCircle2 className="size-3.5" /> success
        </span>
      );
    case "failed":
      return (
        <span className="inline-flex items-center gap-1 text-red-600">
          <XCircle className="size-3.5" /> failed
        </span>
      );
    default:
      return (
        <span className="inline-flex items-center gap-1 text-muted-foreground">
          <Clock className="size-3.5" /> {status}
        </span>
      );
  }
}

function CreateTargetDialog({ orgId, onCreated }: { orgId: string; onCreated: (t: CpBackupTarget) => void }) {
  const [open, setOpen] = React.useState(false);
  const [form, setForm] = React.useState({
    name: "", endpoint: "", bucket: "", region: "", accessKey: "", secretKey: "",
  });
  const [pending, startTransition] = React.useTransition();

  function set<K extends keyof typeof form>(k: K, v: string) {
    setForm((f) => ({ ...f, [k]: v }));
  }

  function submit() {
    if (!form.name || !form.bucket || !form.accessKey || !form.secretKey) {
      toast.error("Name, bucket, access key and secret key are required.");
      return;
    }
    startTransition(async () => {
      try {
        const t = await createBackupTarget({ orgId, ...form });
        toast.success(`Backup target ${t.name} created`);
        setOpen(false);
        setForm({ name: "", endpoint: "", bucket: "", region: "", accessKey: "", secretKey: "" });
        onCreated(t);
      } catch (err) {
        toast.error("Couldn’t create target", { description: errMsg(err) });
      }
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <Plus className="size-4" />
            New target
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New backup target</DialogTitle>
          <DialogDescription>
            Any S3-compatible endpoint (AWS, MinIO, …). The secret key is
            envelope-encrypted at rest; backups themselves are encrypted
            client-side by restic before leaving your server.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="bt-name">Name</Label>
            <Input id="bt-name" value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="prod-backups" />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="bt-endpoint">Endpoint (empty = AWS S3)</Label>
            <Input id="bt-endpoint" value={form.endpoint} onChange={(e) => set("endpoint", e.target.value)} placeholder="https://minio.example.com:9000" />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="bt-bucket">Bucket</Label>
              <Input id="bt-bucket" value={form.bucket} onChange={(e) => set("bucket", e.target.value)} placeholder="sigmahub-backups" />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="bt-region">Region</Label>
              <Input id="bt-region" value={form.region} onChange={(e) => set("region", e.target.value)} placeholder="eu-central-1" />
            </div>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="bt-ak">Access key</Label>
            <Input id="bt-ak" value={form.accessKey} onChange={(e) => set("accessKey", e.target.value)} autoComplete="off" />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="bt-sk">Secret key</Label>
            <Input id="bt-sk" type="password" value={form.secretKey} onChange={(e) => set("secretKey", e.target.value)} autoComplete="off" />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={submit} disabled={pending}>
            {pending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
            Create target
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function RestoreDialog({
  orgId,
  resourceId,
  environmentId,
  serverId,
  servers,
  sourceName,
}: {
  orgId: string;
  resourceId: string;
  environmentId: string;
  serverId: string | null;
  servers: RestoreTargetServer[];
  sourceName: string;
}) {
  const router = useRouter();
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState(`${sourceName}-restore`);
  const [target, setTarget] = React.useState(serverId ?? "");
  const [pending, startTransition] = React.useTransition();

  function submit() {
    const plan = planRestoreTarget(target, servers);
    if (!plan.ok) {
      toast.error("Can’t restore onto that server", { description: plan.reason });
      return;
    }
    startTransition(async () => {
      try {
        const out = await restoreDatabase({
          orgId,
          resourceId,
          name,
          environmentId,
          serverId: plan.serverId,
        });
        toast.success("Restore queued", {
          description: `Loading the latest snapshot into ${out.resource.name}.`,
        });
        setOpen(false);
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t queue restore", { description: errMsg(err) });
      }
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <RefreshCcw className="size-4" />
            Restore to new
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Restore into a new database</DialogTitle>
          <DialogDescription>
            The fire-drill flow: provisions a fresh database (new credentials,
            new port) and loads the latest verified snapshot into it. The
            source database is untouched.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="restore-name">New resource name</Label>
            <Input id="restore-name" value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <RestoreTargetField
            id="restore-target"
            servers={servers}
            value={target}
            onChange={setTarget}
            sourceServerId={serverId}
          />
        </div>
        <DialogFooter>
          <Button onClick={submit} disabled={pending || !name.trim() || !target}>
            {pending ? <Loader2 className="size-4 animate-spin" /> : <RefreshCcw className="size-4" />}
            Queue restore
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Format an ISO instant as a `datetime-local` input value in the viewer's
 *  local time (the browser converts it back to UTC on submit). */
function toLocalInput(iso: string): string {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** P2-5b point-in-time recovery: provision a fresh postgres resource recovered
 *  to a chosen moment. The time picker is bounded by the live recovery window
 *  (the newest archived WAL); the CP re-validates the window server-side. */
function RestorePITRDialog({
  orgId,
  resourceId,
  environmentId,
  serverId,
  servers,
  sourceName,
  maxWalAt,
}: {
  orgId: string;
  resourceId: string;
  environmentId: string;
  serverId: string | null;
  servers: RestoreTargetServer[];
  sourceName: string;
  maxWalAt: string;
}) {
  const router = useRouter();
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState(`${sourceName}-pitr`);
  const [when, setWhen] = React.useState(toLocalInput(maxWalAt));
  const [target, setTarget] = React.useState(serverId ?? "");
  const [pending, startTransition] = React.useTransition();

  function submit() {
    if (!when) {
      toast.error("Pick a recovery time");
      return;
    }
    const plan = planRestoreTarget(target, servers);
    if (!plan.ok) {
      toast.error("Can’t restore onto that server", { description: plan.reason });
      return;
    }
    const targetTime = new Date(when).toISOString();
    startTransition(async () => {
      try {
        const out = await restoreDatabaseToTimestamp({
          orgId,
          resourceId,
          name,
          environmentId,
          serverId: plan.serverId,
          targetTime,
        });
        toast.success("Point-in-time restore queued", {
          description: `Recovering ${out.resource.name} to ${new Date(targetTime).toLocaleString("en-GB")}.`,
        });
        setOpen(false);
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t queue recovery", { description: errMsg(err) });
      }
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <History className="size-4" />
            Restore to time
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Restore to a point in time</DialogTitle>
          <DialogDescription>
            Provisions a fresh database and recovers it to the moment you choose —
            the newest base backup before that time, then WAL replayed up to it.
            The source database is untouched.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="pitr-name">New resource name</Label>
            <Input id="pitr-name" value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="pitr-when">Recover to</Label>
            <Input
              id="pitr-when"
              type="datetime-local"
              value={when}
              max={toLocalInput(maxWalAt)}
              onChange={(e) => setWhen(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              Recoverable up to {new Date(maxWalAt).toLocaleString("en-GB")} — the newest
              archived WAL. Earlier times replay less.
            </p>
          </div>
          <RestoreTargetField
            id="pitr-target"
            servers={servers}
            value={target}
            onChange={setTarget}
            sourceServerId={serverId}
          />
        </div>
        <DialogFooter>
          <Button onClick={submit} disabled={pending || !name.trim() || !when || !target}>
            {pending ? <Loader2 className="size-4 animate-spin" /> : <History className="size-4" />}
            Queue recovery
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** P1-11 backups panel on a database resource: policy target + enable toggle,
 *  run history (backup / restore-verify / restore), and the fire-drill
 *  restore-into-new-resource flow. */
export function DatabaseBackupsPanel({
  orgId,
  resourceId,
  environmentId,
  serverId,
  resourceName,
  policy,
  targets: initialTargets,
  runs: initialRuns,
  canManage,
  servers = [],
  engine,
  pitrWindow,
}: {
  orgId: string;
  resourceId: string;
  environmentId: string;
  serverId: string | null;
  resourceName: string;
  policy: CpBackupPolicy | null;
  targets: CpBackupTarget[];
  runs: CpBackupRun[];
  canManage: boolean;
  /** Servers a restore may be provisioned onto (SIGMA-241). Empty means the
   *  org server list could not be read; the restore then falls back to the
   *  source's own server, which is the old behaviour. */
  servers?: RestoreTargetServer[];
  /** Engine kind — PITR is offered for postgres only (P2-5). */
  engine?: string;
  /** WAL high-water mark: the newest point a PITR restore can currently reach. */
  pitrWindow?: { lastWalAt?: string | null; lastWalSegment?: string } | null;
}) {
  const router = useRouter();
  const [targets, setTargets] = React.useState(initialTargets);
  const [runs, setRuns] = React.useState(initialRuns);
  // The mutations below call router.refresh(), which re-renders the server page
  // and delivers fresh props — but useState keeps the initial snapshot, so the
  // run history would go stale after a restore/policy change (SIGMA-154). Sync
  // local state to props when the props themselves change (render-time prev-prop
  // pattern), while still allowing refreshRuns() to override between refreshes.
  const [prevRuns, setPrevRuns] = React.useState(initialRuns);
  if (initialRuns !== prevRuns) {
    setPrevRuns(initialRuns);
    setRuns(initialRuns);
  }
  const [prevTargets, setPrevTargets] = React.useState(initialTargets);
  if (initialTargets !== prevTargets) {
    setPrevTargets(initialTargets);
    setTargets(initialTargets);
  }
  const [pending, startTransition] = React.useTransition();

  function updatePolicy(input: {
    targetId?: string | null;
    enabled?: boolean;
    pitrEnabled?: boolean;
  }) {
    startTransition(async () => {
      try {
        await setBackupPolicy({ orgId, resourceId, ...input });
        toast.success("Backup policy updated");
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t update policy", { description: errMsg(err) });
      }
    });
  }

  function refreshRuns() {
    startTransition(async () => {
      try {
        setRuns(await listBackupRuns({ orgId, resourceId }));
      } catch (err) {
        toast.error("Couldn’t refresh history", { description: errMsg(err) });
      }
    });
  }

  const hasTarget = Boolean(policy?.targetId);
  const hasSuccess = runs.some((r) => r.kind === "backup" && r.status === "success");
  // SIGMA-241: the restore controls used to be gated on the SOURCE resource
  // having a serverId, which deleted the entire restore path in the one state
  // it exists for — a database whose host is gone. What a restore actually
  // needs is somewhere to land: the source's server, or any server the dialog
  // can aim at.
  const canRestore = canManage && hasSuccess && (Boolean(serverId) || servers.length > 0);

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="inline-flex items-center gap-2">
              <Archive className="size-4" />
              Backups
            </CardTitle>
            <CardDescription>
              Daily restic backups, encrypted before leaving the server. Every
              backup is followed by an automated restore-verify — an unrestored
              backup counts as no backup.
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            {canRestore && (
              <RestoreDialog
                orgId={orgId}
                resourceId={resourceId}
                environmentId={environmentId}
                serverId={serverId}
                servers={servers}
                sourceName={resourceName}
              />
            )}
            <Button variant="ghost" size="sm" onClick={refreshRuns} disabled={pending} aria-label="Refresh history">
              <RefreshCcw className="size-4" />
            </Button>
          </div>
        </div>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {!hasTarget && (
          <p className="inline-flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-2.5 py-2 text-sm text-amber-700 dark:text-amber-400">
            <ShieldAlert className="mt-0.5 size-4 shrink-0" />
            No backup target configured — this database is NOT being backed up.
            Pick or create an S3-compatible target below.
          </p>
        )}

        {canManage && (
          <div className="flex flex-wrap items-center gap-2">
            <Select
              value={policy?.targetId ?? ""}
              onValueChange={(v) => updatePolicy({ targetId: v })}
              disabled={pending}
            >
              <SelectTrigger className="w-56" size="sm">
                <SelectValue placeholder="Choose backup target…" />
              </SelectTrigger>
              <SelectContent>
                {targets.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.name} · {t.bucket}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <CreateTargetDialog orgId={orgId} onCreated={(t) => setTargets((cur) => [...cur, t])} />
            {policy && (
              <label className="ml-auto inline-flex items-center gap-2 text-sm text-muted-foreground">
                <Switch
                  checked={policy.enabled}
                  onCheckedChange={(v) => updatePolicy({ enabled: v })}
                  disabled={pending || !hasTarget}
                />
                Scheduled backups
              </label>
            )}
          </div>
        )}

        {policy && (
          <p className="text-xs text-muted-foreground">
            Retention: keep {policy.keepDaily} daily
            {policy.keepWeekly > 0 && ` / ${policy.keepWeekly} weekly`}
            {policy.keepMonthly > 0 && ` / ${policy.keepMonthly} monthly`} snapshots (GFS).
          </p>
        )}

        {policy && engine === "postgres" && (
          <div className="flex flex-col gap-2 rounded-md border border-border bg-muted/30 p-3">
            <div className="flex items-center justify-between gap-2">
              <div className="flex flex-col">
                <span className="text-sm font-medium text-foreground">
                  Point-in-time recovery
                </span>
                <span className="text-xs text-muted-foreground">
                  Continuous WAL archiving + a daily base backup, so you can restore to any
                  moment — not just the last daily snapshot.
                </span>
              </div>
              {canManage ? (
                <Switch
                  checked={Boolean(policy.pitrEnabled)}
                  onCheckedChange={(v) => updatePolicy({ pitrEnabled: v })}
                  disabled={pending || !hasTarget}
                  aria-label="Point-in-time recovery"
                />
              ) : (
                <span className="text-xs text-muted-foreground">
                  {policy.pitrEnabled ? "On" : "Off"}
                </span>
              )}
            </div>
            {policy.pitrEnabled && (
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-xs text-muted-foreground">
                  {pitrWindow?.lastWalAt
                    ? `Recoverable up to ${new Date(pitrWindow.lastWalAt).toLocaleString("en-GB")} (last archived segment ${pitrWindow.lastWalSegment ?? "—"}).`
                    : "Waiting for the first WAL segment to ship — the recovery window opens once archiving reports in."}
                </p>
                {canManage &&
                  (Boolean(serverId) || servers.length > 0) &&
                  pitrWindow?.lastWalAt && (
                    <RestorePITRDialog
                      orgId={orgId}
                      resourceId={resourceId}
                      environmentId={environmentId}
                      serverId={serverId}
                      servers={servers}
                      sourceName={resourceName}
                      maxWalAt={pitrWindow.lastWalAt}
                    />
                  )}
              </div>
            )}
          </div>
        )}

        {runs.length === 0 ? (
          <p className="text-sm text-muted-foreground">No backup runs yet.</p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Kind</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Snapshot</TableHead>
                  <TableHead>When</TableHead>
                  <TableHead>Detail</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {runs.map((r) => (
                  <TableRow key={r.id}>
                    <TableCell className="font-medium">
                      {r.kind === "verify" ? "restore-verify" : r.kind}
                    </TableCell>
                    <TableCell>
                      <RunStatus status={r.status} />
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {r.snapshotId ? r.snapshotId.slice(0, 8) : "—"}
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                      {new Date(r.createdAt).toLocaleString()}
                    </TableCell>
                    <TableCell className="max-w-64 truncate text-xs text-muted-foreground" title={r.detail}>
                      {r.detail || "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
