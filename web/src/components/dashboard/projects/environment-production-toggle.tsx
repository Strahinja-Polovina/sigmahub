"use client";

import * as React from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Switch } from "@/components/ui/switch";
import { setEnvironmentProduction } from "@/server/actions/projects";

/** Displays and (Project Admin+) edits an environment's production flag —
 *  the seed for database backup retention. Previously the flag was silently
 *  inferred from the environment's NAME at creation and never shown or
 *  editable again, so a prod env named "live" was stuck with non-production
 *  backups forever (SIGMA-190). */
export function EnvironmentProductionToggle({
  environmentId,
  production,
  canManage,
}: {
  environmentId: string;
  production: boolean;
  canManage: boolean;
}) {
  const [pending, startTransition] = React.useTransition();

  if (!canManage) {
    return production ? <Badge>Production</Badge> : null;
  }

  function toggle(next: boolean) {
    startTransition(async () => {
      try {
        await setEnvironmentProduction({ environmentId, production: next });
        toast.success(
          next
            ? "Marked as production — new databases keep 30 daily backups"
            : "Unmarked as production — new databases use the 7/4/6 default"
        );
      } catch (err) {
        toast.error("Couldn’t update environment", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <span className="inline-flex items-center gap-2">
      {production && <Badge>Production</Badge>}
      <label className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
        <Switch
          checked={production}
          disabled={pending}
          onCheckedChange={(v) => toggle(Boolean(v))}
          aria-label="Production environment"
        />
        Production
      </label>
    </span>
  );
}
