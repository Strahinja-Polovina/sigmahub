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

export function StatusDot({
  status,
  className,
}: {
  status: Status;
  className?: string;
}) {
  const meta = STATUS_META[status];
  return (
    <span
      className={cn("inline-block size-1.5 shrink-0 rounded-full", meta.dot, className)}
      aria-hidden
    />
  );
}

export function StatusBadge({ status }: { status: Status }) {
  const meta = STATUS_META[status];
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
