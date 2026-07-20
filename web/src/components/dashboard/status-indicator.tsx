import { cn } from "@/lib/utils";
import type { Status } from "@/lib/mock";

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

// The control plane reports a resource status as an object ({ state: "applied" }),
// and both resources and servers use CP state vocabulary that doesn't line up 1:1
// with the UI's Status enum. Normalize both shapes here so an unmapped value
// degrades to a neutral badge instead of crashing the whole page.
const STATE_ALIASES: Record<string, Status> = {
  applied: "running",
  ready: "running",
  active: "running",
  pending: "provisioning",
  creating: "provisioning",
  deploying: "provisioning",
  building: "provisioning",
  failed: "error",
};

function resolveMeta(status: unknown) {
  let key = "";
  if (typeof status === "string") key = status;
  else if (status && typeof status === "object" && "state" in status) {
    key = String((status as { state: unknown }).state ?? "");
  }
  const norm = STATE_ALIASES[key] ?? (key as Status);
  return STATUS_META[norm] ?? FALLBACK_META;
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
