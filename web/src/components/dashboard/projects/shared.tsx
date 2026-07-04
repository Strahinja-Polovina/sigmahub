import { Badge } from "@/components/ui/badge";
import type { ResourceKind, ServerType } from "@/lib/mock";
import { KIND_LABELS, SERVER_TYPE_LABELS } from "@/lib/constants";

// Re-export for backwards-compatible imports.
export { KIND_LABELS, SERVER_TYPE_LABELS };
export { formatDate, formatDateTime } from "@/lib/formatters";

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
