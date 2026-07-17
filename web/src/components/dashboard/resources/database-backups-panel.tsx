"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import {
  Archive,
  CheckCircle2,
  Clock,
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
  setBackupPolicy,
} from "@/server/actions/backups";
import type { CpBackupPolicy, CpBackupRun, CpBackupTarget } from "@/server/cp";

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
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
  sourceName,
}: {
  orgId: string;
  resourceId: string;
  environmentId: string;
  serverId: string;
  sourceName: string;
}) {
  const router = useRouter();
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState(`${sourceName}-restore`);
  const [pending, startTransition] = React.useTransition();

  function submit() {
    startTransition(async () => {
      try {
        const out = await restoreDatabase({ orgId, resourceId, name, environmentId, serverId });
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
        <div className="grid gap-1.5">
          <Label htmlFor="restore-name">New resource name</Label>
          <Input id="restore-name" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <DialogFooter>
          <Button onClick={submit} disabled={pending || !name.trim()}>
            {pending ? <Loader2 className="size-4 animate-spin" /> : <RefreshCcw className="size-4" />}
            Queue restore
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
  /** Engine kind — PITR is offered for postgres only (P2-5). */
  engine?: string;
  /** WAL high-water mark: the newest point a PITR restore can currently reach. */
  pitrWindow?: { lastWalAt?: string | null; lastWalSegment?: string } | null;
}) {
  const router = useRouter();
  const [targets, setTargets] = React.useState(initialTargets);
  const [runs, setRuns] = React.useState(initialRuns);
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
            {canManage && serverId && hasSuccess && (
              <RestoreDialog
                orgId={orgId}
                resourceId={resourceId}
                environmentId={environmentId}
                serverId={serverId}
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
              <p className="text-xs text-muted-foreground">
                {pitrWindow?.lastWalAt
                  ? `Recoverable up to ${new Date(pitrWindow.lastWalAt).toLocaleString("en-GB")} (last archived segment ${pitrWindow.lastWalSegment ?? "—"}).`
                  : "Waiting for the first WAL segment to ship — the recovery window opens once archiving reports in."}
              </p>
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
