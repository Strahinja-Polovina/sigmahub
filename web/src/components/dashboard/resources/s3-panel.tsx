"use client";

import * as React from "react";
import { Copy, Eye, EyeOff, FolderPlus, HardDrive, KeyRound, Loader2, Lock, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  S3_ENGINE_NAMES,
  type S3Engine,
} from "@/lib/server-catalog.generated";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  revealS3Connection,
  listBuckets,
  createBucket,
  deleteBucket,
  createBucketKey,
  revealBucketKey,
} from "@/server/actions/s3";
import { ControlPlaneNote } from "@/components/dashboard/control-plane-note";
import type { CpS3Info, CpS3Connection, CpBucket } from "@/server/cp";

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

// Human labels for the object-storage engines (P2-2). SeaweedFS is the
// Apache-2.0 hedge against MinIO's AGPL license; both speak the S3 API.
//
// Keyed on S3Engine rather than on string, so an engine added to the control
// plane's catalog stops this file compiling instead of rendering its raw id at
// a customer. The engines themselves are the catalog's; only the words are ours.
const ENGINE_LABELS: Record<S3Engine, string> = {
  minio: "MinIO",
  seaweedfs: "SeaweedFS",
};

// The honest support matrix: what maps 1:1 across engines and what does not,
// so an operator picking SeaweedFS knows exactly what stays the same. Mirrors
// the database-engine matrix — no silent feature gaps.
//
// A row is a total Record over the engines for the same reason as above: a
// third engine used to get no column here at all, which reads as "this table
// covers everything" while quietly covering less.
const SUPPORT_MATRIX: { capability: string; support: Record<S3Engine, string> }[] = [
  { capability: "S3 API over the mesh", support: { minio: "yes", seaweedfs: "yes" } },
  { capability: "Root credentials (env)", support: { minio: "yes", seaweedfs: "yes" } },
  { capability: "Buckets via any S3 client", support: { minio: "yes", seaweedfs: "yes" } },
  { capability: "In-dashboard bucket CRUD", support: { minio: "yes", seaweedfs: "yes" } },
  { capability: "Per-bucket keys + quotas", support: { minio: "yes*", seaweedfs: "yes" } },
  { capability: "Built-in web console", support: { minio: "off (disabled)", seaweedfs: "n/a" } },
  { capability: "License", support: { minio: "AGPL-3.0", seaweedfs: "Apache-2.0" } },
];

function copy(value: string, label: string) {
  void navigator.clipboard.writeText(value).then(
    () => toast.success(`${label} copied`),
    () => toast.error(`Couldn’t copy ${label.toLowerCase()}`)
  );
}

function ConnRow({ label, value, copyable = false }: {
  label: string;
  value: string;
  copyable?: boolean;
}) {
  return (
    <div className="flex items-center justify-between gap-4 py-2">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className="inline-flex min-w-0 items-center gap-1.5">
        <span className="truncate font-mono text-sm">{value}</span>
        {copyable && (
          <Button
            variant="ghost"
            size="icon"
            className="size-6 shrink-0"
            onClick={() => copy(value, label)}
            aria-label={`Copy ${label}`}
          >
            <Copy className="size-3.5" />
          </Button>
        )}
      </span>
    </div>
  );
}

function fmtBytes(n: number): string {
  if (n <= 0) return "no limit";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
}

function bucketStatusTone(status: string): string {
  switch (status) {
    case "active":
      return "text-emerald-700 dark:text-emerald-400";
    case "deleting":
      return "text-red-600";
    default:
      return "text-muted-foreground";
  }
}

/** P2-1b bucket management (SIGMA-65): list/create/delete + per-bucket least-
 *  privilege keys, all applied over the mesh by the agent's s3.configure op —
 *  the root credential is never revealed for these. Buckets land as
 *  `provisioning` and flip to `active` once the agent reports the op applied. */
function BucketManager({
  orgId,
  resourceId,
  canManage,
}: {
  orgId: string;
  resourceId: string;
  canManage: boolean;
}) {
  const [buckets, setBuckets] = React.useState<CpBucket[] | null>(null);
  const [name, setName] = React.useState("");
  const [pending, startTransition] = React.useTransition();
  // SIGMA-311: the trash icon used to call deleteBucket directly, so one click
  // on a 28px control queued the destruction of a bucket and every object in
  // it. Nothing about the icon says which bucket it belongs to once the pointer
  // is over it, and there is no undo on the other side. It now only names the
  // bucket awaiting confirmation; the delete itself needs a second click.
  const [confirmDelete, setConfirmDelete] = React.useState<string | null>(null);
  const [revealedKey, setRevealedKey] = React.useState<
    { bucket: string; accessKey: string; secretKey: string } | null
  >(null);

  const refresh = React.useCallback(() => {
    listBuckets({ orgId, resourceId })
      .then(setBuckets)
      .catch(() => setBuckets([]));
  }, [orgId, resourceId]);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  function add() {
    const n = name.trim();
    if (!n) return;
    startTransition(async () => {
      try {
        await createBucket({ orgId, resourceId, name: n });
        setName("");
        toast.success(`Bucket ${n} queued`, { description: "Provisioning over the mesh." });
        refresh();
      } catch (err) {
        toast.error("Couldn’t create bucket", { description: errMsg(err) });
      }
    });
  }

  function remove(bucket: string) {
    startTransition(async () => {
      try {
        await deleteBucket({ orgId, resourceId, bucket });
        setConfirmDelete(null);
        toast.success(`Bucket ${bucket} deletion queued`);
        refresh();
      } catch (err) {
        toast.error("Couldn’t delete bucket", { description: errMsg(err) });
      }
    });
  }

  function mintKey(bucket: string) {
    startTransition(async () => {
      try {
        const { accessKey } = await createBucketKey({ orgId, resourceId, bucket });
        // SIGMA-313: this used to promise the secret would be "shown once it's
        // active" — and then show it nowhere, because the create response
        // carries only the key id and the mint button disappears as soon as the
        // key is recorded. The copy now points at the control that exists.
        toast.success("Per-bucket key created", {
          description: `Access key ${accessKey}. Use Reveal on the bucket row to copy its secret.`,
        });
        refresh();
      } catch (err) {
        toast.error("Couldn’t create key", { description: errMsg(err) });
      }
    });
  }

  // SIGMA-313: a minted key without a way back to its secret is a credential
  // nobody can use. This is the audited reveal — the same shape as the root
  // credential's, scoped to one bucket.
  function reveal(bucket: string) {
    startTransition(async () => {
      try {
        const key = await revealBucketKey({ orgId, resourceId, bucket });
        setRevealedKey(key);
        toast.message("Bucket key revealed", {
          description: "This reveal was written to the audit log.",
        });
      } catch (err) {
        toast.error("Couldn’t reveal the bucket key", { description: errMsg(err) });
      }
    });
  }

  return (
    <div className="flex flex-col gap-3 rounded-md border border-border bg-muted/30 p-3">
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium text-foreground">Buckets</span>
        {canManage && (
          <div className="flex items-center gap-1.5">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value.toLowerCase())}
              onKeyDown={(e) => e.key === "Enter" && add()}
              placeholder="bucket-name"
              className="h-7 w-40 font-mono text-xs"
            />
            <Button size="sm" className="h-7" onClick={add} disabled={pending || !name.trim()}>
              {pending ? <Loader2 className="size-3.5 animate-spin" /> : <FolderPlus className="size-3.5" />}
              Create
            </Button>
          </div>
        )}
      </div>

      {buckets === null ? (
        <p className="text-xs text-muted-foreground">Loading buckets…</p>
      ) : buckets.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          No buckets yet. Create one above, or with any S3 client using the credentials.
        </p>
      ) : (
        <div className="flex flex-col divide-y divide-border">
          {buckets.map((b) => (
            <div key={b.id} className="flex items-center justify-between gap-3 py-2">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="truncate font-mono text-sm text-foreground">{b.name}</span>
                  <span className={`text-[11px] ${bucketStatusTone(b.status)}`}>{b.status}</span>
                </div>
                <p className="text-[11px] text-muted-foreground">
                  quota {fmtBytes(b.quotaBytes)}
                  {b.accessKey ? ` · key ${b.accessKey}` : " · no scoped key"}
                </p>
              </div>
              {canManage && (
                <div className="flex shrink-0 items-center gap-1">
                  {b.accessKey ? (
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-7"
                      aria-label={`Reveal key for ${b.name}`}
                      onClick={() => reveal(b.name)}
                      disabled={pending}
                      title="Show this bucket's scoped secret (audited)"
                    >
                      <Eye className="size-3.5" />
                      Reveal
                    </Button>
                  ) : (
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-7"
                      onClick={() => mintKey(b.name)}
                      disabled={pending || b.status !== "active"}
                      title="Create a least-privilege key scoped to this bucket"
                    >
                      <KeyRound className="size-3.5" />
                      Key
                    </Button>
                  )}
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={`Delete ${b.name}`}
                    onClick={() => setConfirmDelete(b.name)}
                    disabled={pending}
                  >
                    <Trash2 className="size-3.5 text-muted-foreground" />
                  </Button>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      {/* SIGMA-313: the scoped credential, in full, for the operator who minted
          it. Re-openable from the row — this is a reveal, not a one-time show,
          because the secret survives at rest and the audit log records each
          read. */}
      <Dialog
        open={revealedKey !== null}
        onOpenChange={(next) => {
          if (!next) setRevealedKey(null);
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Key for bucket {revealedKey?.bucket}</DialogTitle>
            <DialogDescription>
              A least-privilege credential scoped to this bucket only. This reveal was
              written to the audit log.
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col divide-y divide-border">
            <ConnRow label="Access key" value={revealedKey?.accessKey ?? ""} copyable />
            <ConnRow label="Secret key" value={revealedKey?.secretKey ?? ""} copyable />
          </div>
          <DialogFooter>
            <DialogClose render={<Button type="button">Done</Button>} />
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={confirmDelete !== null}
        onOpenChange={(next) => {
          if (pending) return;
          if (!next) setConfirmDelete(null);
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Delete bucket {confirmDelete}?</DialogTitle>
            <DialogDescription>
              The agent removes <span className="font-mono">{confirmDelete}</span> from the
              engine and everything in it — every object stored in this bucket goes with it,
              and nothing here keeps a copy. Any scoped key issued for the bucket stops
              working too.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
              Cancel
            </DialogClose>
            <Button
              variant="destructive"
              onClick={() => confirmDelete && remove(confirmDelete)}
              disabled={pending}
            >
              {pending && <Loader2 className="size-4 animate-spin" />}
              Delete bucket
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/** P2-1 S3 storage panel: mesh-only endpoint + access key for every member,
 *  the audited secret-key reveal for Project Admin+. Buckets are managed with
 *  any S3 client for now — in-dashboard bucket CRUD is a follow-up. */
export function S3Panel({
  orgId,
  resourceId,
  info,
  canManage,
  simulated = false,
}: {
  orgId: string;
  resourceId: string;
  info: CpS3Info;
  canManage: boolean;
  /** Demo mode: the endpoint and key pair are derived from the resource rather
   *  than reported by a running MinIO, and bucket management — which is the
   *  agent reconfiguring that MinIO over the mesh — has nothing to talk to. */
  simulated?: boolean;
}) {
  const [conn, setConn] = React.useState<CpS3Connection | null>(null);
  const [revealed, setRevealed] = React.useState(false);
  const [pending, startTransition] = React.useTransition();

  function reveal() {
    if (conn) {
      setRevealed((v) => !v);
      return;
    }
    startTransition(async () => {
      try {
        const c = await revealS3Connection({ orgId, resourceId });
        setConn(c);
        setRevealed(true);
        toast.message("Credentials revealed", {
          description: "This reveal was written to the audit log.",
        });
      } catch (err) {
        toast.error("Couldn’t reveal credentials", { description: errMsg(err) });
      }
    });
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="inline-flex items-center gap-2">
              <HardDrive className="size-4" />
              Object storage
            </CardTitle>
            <CardDescription>
              S3-compatible API, reachable only across your org’s WireGuard mesh — the
              port is never published on a public interface.
            </CardDescription>
          </div>
          <Badge variant="outline" className="shrink-0">
            <Lock className="size-3" />
            Mesh-only
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="flex flex-col divide-y divide-border">
          <div className="flex items-center justify-between gap-4 py-2">
            <span className="text-sm text-muted-foreground">Engine</span>
            <span className="inline-flex min-w-0 items-center gap-2">
              <Badge variant="secondary" className="shrink-0">
                {ENGINE_LABELS[info.engine as S3Engine] ?? info.engine}
              </Badge>
              <span className="truncate font-mono text-xs text-muted-foreground">
                {info.image}
              </span>
            </span>
          </div>
          <ConnRow
            label="Endpoint"
            value={info.endpoint || "pending mesh enrollment"}
            copyable={Boolean(info.endpoint)}
          />
          <ConnRow label="Access key" value={info.accessKey} copyable />
          <div className="flex items-center justify-between gap-4 py-2">
            <span className="text-sm text-muted-foreground">Secret key</span>
            <span className="inline-flex min-w-0 items-center gap-1.5">
              <span className="truncate font-mono text-sm">
                {revealed && conn ? conn.secretKey : "••••••••••••"}
              </span>
              {revealed && conn && (
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-6 shrink-0"
                  onClick={() => copy(conn.secretKey, "Secret key")}
                  aria-label="Copy secret key"
                >
                  <Copy className="size-3.5" />
                </Button>
              )}
              {canManage ? (
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7"
                  onClick={reveal}
                  disabled={pending}
                >
                  {pending ? (
                    <Loader2 className="size-3.5 animate-spin" />
                  ) : revealed ? (
                    <EyeOff className="size-3.5" />
                  ) : (
                    <Eye className="size-3.5" />
                  )}
                  {revealed ? "Hide" : "Reveal"}
                </Button>
              ) : (
                <span className="text-xs text-muted-foreground">Project Admin+</span>
              )}
            </span>
          </div>
        </div>

        {simulated ? (
          <>
            <p className="text-xs text-muted-foreground">
              No object store is running behind this: the endpoint and access key are
              generated from the resource so the flow is walkable, and the secret starts
              with <code className="font-mono">demo_</code> so it cannot be mistaken for
              one.
            </p>
            {/* Buckets are the agent's s3.configure op talking to a real engine.
                A list that only ever agreed with itself would teach nothing, so
                the panel says what the capability is instead (SIGMA-215). */}
            <ControlPlaneNote title="Buckets are created on the engine itself">
              With a control plane, this panel creates and deletes buckets, sets
              per-bucket quotas and mints scoped access keys — the agent applies each
              change to the running engine over the mesh and the panel reflects it as it
              converges. There is no engine here to apply them to.
            </ControlPlaneNote>
          </>
        ) : (
          /* P2-1b bucket management over the mesh. */
          <BucketManager orgId={orgId} resourceId={resourceId} canManage={canManage} />
        )}

        {info.endpoint && (
          <div className="rounded-md border border-border bg-muted/40 p-3">
            <p className="text-xs text-muted-foreground">
              Also works with any S3 client from machines on the mesh, e.g.:
            </p>
            <pre className="mt-1.5 overflow-x-auto font-mono text-xs text-foreground">
              {`aws s3 mb s3://my-bucket --endpoint-url ${info.endpoint}`}
            </pre>
          </div>
        )}

        {/* P2-2 honest engine support matrix — what maps 1:1 and what doesn't. */}
        <div className="overflow-x-auto rounded-md border border-border">
          <table className="w-full text-xs">
            <thead>
              {/* Columns come from the catalog, so a new engine gets one
                  rather than being left out of a table that claims to compare
                  them all. */}
              <tr className="border-b border-border bg-muted/40 text-left text-muted-foreground">
                <th className="px-3 py-2 font-medium">Capability</th>
                {S3_ENGINE_NAMES.map((engine) => (
                  <th key={engine} className="px-3 py-2 font-medium">
                    {ENGINE_LABELS[engine]}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {SUPPORT_MATRIX.map((row) => (
                <tr key={row.capability} className="border-b border-border last:border-0">
                  <td className="px-3 py-2 text-foreground">{row.capability}</td>
                  {S3_ENGINE_NAMES.map((engine) => (
                    <td
                      key={engine}
                      className={
                        row.support[engine] === "yes"
                          ? "px-3 py-2 font-medium text-emerald-600"
                          : "px-3 py-2 text-muted-foreground"
                      }
                    >
                      {row.support[engine]}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="text-[11px] text-muted-foreground">
          * MinIO per-bucket keys + quotas run through the MinIO Admin API; the exact
          wire form for the pinned release is validated on staging.
        </p>
      </CardContent>
    </Card>
  );
}
