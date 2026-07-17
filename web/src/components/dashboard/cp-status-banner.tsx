import { AlertTriangle } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import type { CpSyncStatus } from "@/server/cp-sync";

/** Dashboard-wide honesty banner (SIGMA-56): when the control plane is
 *  unreachable the pages keep rendering from the local mirror, and this says
 *  so explicitly instead of leaving silently stale (or empty) lists. */
export function CpStatusBanner({ status }: { status: CpSyncStatus }) {
  if (!status.enabled || status.healthy) return null;
  const lastSync = status.lastSyncAt
    ? new Date(status.lastSyncAt).toLocaleString("en-GB")
    : null;
  return (
    <div className="px-4 pt-4 lg:px-6">
      <Alert variant="destructive">
        <AlertTriangle />
        <AlertTitle>Control plane unreachable</AlertTitle>
        <AlertDescription>
          Showing the last synced state — it may be stale.{" "}
          {lastSync
            ? `Last successful sync: ${lastSync}.`
            : "No successful sync yet."}
          {status.error ? ` (${status.error})` : null}
        </AlertDescription>
      </Alert>
    </div>
  );
}
