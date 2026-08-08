import { Badge } from "@/components/ui/badge";
import type { ResourceKind, ServerType } from "@/lib/mock";

// Labels come from the control plane's catalog (generated, SIGMA-198). Both are
// re-exported because callers across the project pages import them from here.
import {
  RESOURCE_KIND_LABELS as KIND_LABELS,
  SERVER_TYPE_LABELS,
} from "@/lib/server-catalog.generated";

export { KIND_LABELS, SERVER_TYPE_LABELS };

export function KindBadge({ kind }: { kind: ResourceKind }) {
  return (
    <Badge variant="outline" className="font-mono">
      {KIND_LABELS[kind]}
    </Badge>
  );
}

export function ServerTypeBadge({ type }: { type: ServerType }) {
  return (
    <Badge variant="secondary" className="font-normal">
      {SERVER_TYPE_LABELS[type]}
    </Badge>
  );
}

export function formatDate(iso: string | Date) {
  return new Date(iso).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

export function formatDateTime(iso: string | Date) {
  return new Date(iso).toLocaleString("en-GB", {
    day: "numeric",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
  });
}
