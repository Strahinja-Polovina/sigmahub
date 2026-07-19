"use client";

import * as React from "react";
import { Copy, Eye, EyeOff, FolderPlus, HardDrive, KeyRound, Loader2, Lock, Trash2 } from "lucide-react";
import { toast } from "sonner";

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
  revealS3Connection,
  listBuckets,
  createBucket,
  deleteBucket,
  createBucketKey,
} from "@/server/actions/s3";
import type { CpS3Info, CpS3Connection, CpBucket } from "@/server/cp";

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

// Human labels for the object-storage engines (P2-2). SeaweedFS is the
// Apache-2.0 hedge against MinIO's AGPL license; both speak the S3 API.
const ENGINE_LABELS: Record<string, string> = {
  minio: "MinIO",
  seaweedfs: "SeaweedFS",
};

// The honest support matrix: what maps 1:1 across engines and what does not,
// so an operator picking SeaweedFS knows exactly what stays the same. Mirrors
// the database-engine matrix — no silent feature gaps.
const SUPPORT_MATRIX: { capability: string; minio: string; seaweedfs: string }[] = [
  { capability: "S3 API over the mesh", minio: "yes", seaweedfs: "yes" },
  { capability: "Root credentials (env)", minio: "yes", seaweedfs: "yes" },
  { capability: "Buckets via any S3 client", minio: "yes", seaweedfs: "yes" },
  { capability: "In-dashboard bucket CRUD", minio: "yes", seaweedfs: "yes" },
  { capability: "Per-bucket keys + quotas", minio: "yes*", seaweedfs: "yes" },
  { capability: "Built-in web console", minio: "off (disabled)", seaweedfs: "n/a" },
  { capability: "License", minio: "AGPL-3.0", seaweedfs: "Apache-2.0" },
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
        toast.success("Per-bucket key created", {
          description: `Access key ${accessKey}. The secret is provisioned on the engine and shown once it’s active.`,
        });
        refresh();
      } catch (err) {
        toast.error("Couldn’t create key", { description: errMsg(err) });
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
                  {!b.accessKey && (
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
                    onClick={() => remove(b.name)}
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
}: {
  orgId: string;
  resourceId: string;
  info: CpS3Info;
  canManage: boolean;
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
                {ENGINE_LABELS[info.engine] ?? info.engine}
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

        {/* P2-1b bucket management over the mesh. */}
        <BucketManager orgId={orgId} resourceId={resourceId} canManage={canManage} />

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
              <tr className="border-b border-border bg-muted/40 text-left text-muted-foreground">
                <th className="px-3 py-2 font-medium">Capability</th>
                <th className="px-3 py-2 font-medium">MinIO</th>
                <th className="px-3 py-2 font-medium">SeaweedFS</th>
              </tr>
            </thead>
            <tbody>
              {SUPPORT_MATRIX.map((row) => (
                <tr key={row.capability} className="border-b border-border last:border-0">
                  <td className="px-3 py-2 text-foreground">{row.capability}</td>
                  <td
                    className={
                      row.minio === "yes"
                        ? "px-3 py-2 font-medium text-emerald-600"
                        : "px-3 py-2 text-muted-foreground"
                    }
                  >
                    {row.minio}
                  </td>
                  <td
                    className={
                      row.seaweedfs === "yes"
                        ? "px-3 py-2 font-medium text-emerald-600"
                        : "px-3 py-2 text-muted-foreground"
                    }
                  >
                    {row.seaweedfs}
                  </td>
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
