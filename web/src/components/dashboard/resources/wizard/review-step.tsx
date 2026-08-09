"use client";

import * as React from "react";
import { CircleAlert, Pencil } from "lucide-react";

import { Button } from "@/components/ui/button";
import { blockingGaps, reviewSummary, type ReviewInput } from "@/lib/wizard/review";
import type { WizardStepId } from "@/lib/wizard/steps";

/**
 * One review screen before anything is created (SIGMA-211).
 *
 * The old flow's last screen before Deploy was the variables table, so the last
 * thing a user saw was never the thing they were about to make. Every row here
 * jumps back to the step that set it, because "wait, that's staging" is only
 * useful if it is one click from being fixed.
 */
export function ReviewStep({
  input,
  onJump,
}: {
  input: ReviewInput;
  onJump: (step: WizardStepId) => void;
}) {
  const rows = reviewSummary(input);
  const gaps = blockingGaps(input);

  return (
    <div className="flex flex-col gap-4">
      <dl className="divide-y divide-border rounded-lg border border-border bg-card">
        {rows.map((row) => (
          <div key={row.label} className="flex items-start gap-3 px-3 py-2.5">
            <dt className="w-28 shrink-0 pt-0.5 text-xs text-muted-foreground">{row.label}</dt>
            <dd className="min-w-0 flex-1">
              <p className="font-mono text-sm break-words text-foreground">{row.value}</p>
              {row.hint && <p className="text-xs text-muted-foreground">{row.hint}</p>}
            </dd>
            {row.step && (
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Change ${row.label.toLowerCase()}`}
                onClick={() => onJump(row.step as WizardStepId)}
              >
                <Pencil className="size-3.5 text-muted-foreground" />
              </Button>
            )}
          </div>
        ))}
      </dl>

      {gaps.length > 0 && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
          <div className="min-w-0 text-xs">
            <p className="font-medium text-destructive">Not ready to deploy yet.</p>
            <ul className="mt-1 flex flex-col gap-0.5 text-destructive/90">
              {gaps.map((g) => (
                <li key={g}>{g}</li>
              ))}
            </ul>
          </div>
        </div>
      )}
    </div>
  );
}
