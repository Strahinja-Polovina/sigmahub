import { cn } from "@/lib/utils";
import type { Status } from "@/lib/mock";
import { normalizeStatus } from "@/lib/status";

// Maps a resource/server Status to a small colored dot + label.
// running=emerald, degraded=amber, error/stopped=red, provisioning=blue.
const STATUS_META: Record<Status, { label: string; dot: string; text: string }> = {
  running: { label: "Running", dot: "bg-emerald-500", text: "text-emerald-700" },
  degraded: { label: "Degraded", dot: "bg-amber-500", text: "text-amber-700" },
  provisioning: { label: "Provisioning", dot: "bg-blue-500", text: "text-blue-700" },
  stopped: { label: "Stopped", dot: "bg-red-500", text: "text-red-700" },
  error: { label: "Error", dot: "bg-red-500", text: "text-red-700" },
};

const FALLBACK_META = { label: "Unknown", dot: "bg-muted-foreground", text: "text-muted-foreground" };

// Some CP states share a UI Status but deserve their own wording: an
// unreachable server is red like `stopped`, but "Stopped" implies someone
// stopped it, while this means the agent went silent (SIGMA-184). A `skipped`
// op means the deploy never ran because a prerequisite failed (SIGMA-189).
const RAW_LABELS: Record<string, string> = {
  unreachable: "Unreachable",
  skipped: "Not deployed",
  // The host installed and is heartbeating; what is wrong is the TYPE it was
  // enrolled as (SIGMA-203). "Error" would send the operator hunting for a
  // crash instead of reading the sentence next to this badge.
  incompatible: "Incompatible",
};

function rawKey(status: unknown): string {
  if (typeof status === "string") return status;
  if (status && typeof status === "object" && "state" in status) {
    return String((status as { state: unknown }).state ?? "");
  }
  return "";
}

// Translation lives in @/lib/status so the mirror writer and this renderer share
// one vocabulary — see the comment there for why a render-only map was a bug.
function resolveMeta(status: unknown) {
  const norm = normalizeStatus(status);
  if (!norm) return FALLBACK_META;
  const meta = STATUS_META[norm];
  const override = RAW_LABELS[rawKey(status)];
  return override ? { ...meta, label: override } : meta;
}

export function StatusDot({
  status,
  className,
}: {
  status: Status;
  className?: string;
}) {
  const meta = resolveMeta(status);
  return (
    <span
      className={cn("inline-block size-1.5 shrink-0 rounded-full", meta.dot, className)}
      aria-hidden
    />
  );
}

export function StatusBadge({ status }: { status: Status }) {
  const meta = resolveMeta(status);
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs font-medium",
        meta.text
      )}
    >
      <span className={cn("size-1.5 rounded-full", meta.dot)} aria-hidden />
      {meta.label}
    </span>
  );
}
