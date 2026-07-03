import type { DeployStatus } from "@/lib/mock";
import { DEPLOY_STATUS_META } from "./resource-meta";

export function DeployStatusBadge({ status }: { status: DeployStatus }) {
  const meta = DEPLOY_STATUS_META[status];
  return (
    <span
      className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs font-medium ${meta.text}`}
    >
      <span className={`size-1.5 rounded-full ${meta.dot}`} aria-hidden />
      {meta.label}
    </span>
  );
}
