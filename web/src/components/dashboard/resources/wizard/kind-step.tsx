"use client";

import * as React from "react";
import Link from "next/link";
import {
  Check,
  ChevronRight,
  CircleAlert,
  Cpu,
  Database,
  ExternalLink,
  GitBranch,
  HardDrive,
} from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import {
  RESOURCE_CATEGORIES,
  RESOURCE_CATEGORY_CATALOG,
  RESOURCE_KIND_LABELS,
  type ResourceCategoryId,
  type ResourceKind,
} from "@/lib/server-catalog.generated";
import {
  categoryAvailability,
  kindAvailability,
  type KindAvailability,
  type TargetInventory,
} from "@/lib/wizard/availability";
import { kindPickerPhase } from "@/lib/wizard/steps";
import { resourceNameError } from "@/lib/wizard/managed";

/**
 * The only thing step 1 still keeps in the dashboard: a glyph per category.
 *
 * Everything else it renders — which categories exist, what they are called,
 * the line under each card, which kinds sit inside one and in what order — is
 * the control plane's catalog (SIGMA-198). A lucide component is not a thing
 * that file can name, so this stays; typing it as a Record keyed on the
 * generated union is what makes it safe to stay, because a category added to
 * the catalog fails `tsc` HERE, at the omission, rather than rendering a card
 * with no icon.
 *
 * The kinds inside a category draw their category's icon. Four database rows
 * with four different glyphs would read as four different kinds of thing, and
 * they are one thing with four engines.
 */
const CATEGORY_ICONS: Record<ResourceCategoryId, React.ElementType> = {
  application: GitBranch,
  database: Database,
  model: Cpu,
  storage: HardDrive,
};

/**
 * Step 1: pick a category, then the kind inside it.
 *
 * The kinds used to sit here flat, so postgres, mysql, mongodb and redis stood
 * beside "Application" as its peers. They are not peers — three of them are the
 * same decision made again — and the flat grid was a wall that grew a row with
 * every kind added.
 *
 * The second list is shown ONLY for a category holding more than one kind. That
 * is the whole point of the screen and not an optimization of it: today
 * Database is the only such category, and Application resolving straight
 * through to its single kind is what keeps this from being the extra click the
 * wizard rework exists to delete. Availability is still decided here rather
 * than at step 4, now at both altitudes.
 */
export function KindStep({
  category,
  kind,
  onPickCategory,
  onPickKind,
  inventory,
  name,
  onNameChange,
}: {
  category: ResourceCategoryId | null;
  kind: ResourceKind | null;
  onPickCategory: (id: ResourceCategoryId) => void;
  onPickKind: (k: ResourceKind) => void;
  inventory: TargetInventory;
  name: string;
  onNameChange: (v: string) => void;
}) {
  const nameProblem = kind ? resourceNameError(name) : null;
  const availability = kind ? kindAvailability(kind, inventory) : null;
  // Null on the first face, which is also what a category resolved to its only
  // kind shows: it never opened a list, so there is nothing to be inside of.
  const openCategory =
    category && kindPickerPhase(category) === "kinds" ? RESOURCE_CATEGORY_CATALOG[category] : null;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1.5">
        <Label className="text-xs text-muted-foreground">
          {openCategory ? `Which ${openCategory.label.toLowerCase()}?` : "What are you deploying?"}
        </Label>
        <div className="grid gap-1.5 sm:grid-cols-2">
          {openCategory
            ? openCategory.kinds.map((k) => (
                <PickerCard
                  key={k}
                  icon={CATEGORY_ICONS[openCategory.id]}
                  label={RESOURCE_KIND_LABELS[k]}
                  availability={kindAvailability(k, inventory)}
                  selected={kind === k}
                  onPick={() => onPickKind(k)}
                />
              ))
            : RESOURCE_CATEGORIES.map((id) => {
                const spec = RESOURCE_CATEGORY_CATALOG[id];
                return (
                  <PickerCard
                    key={id}
                    icon={CATEGORY_ICONS[id]}
                    label={spec.label}
                    detail={spec.hint}
                    availability={categoryAvailability(id, inventory)}
                    // A category that opens a list is never itself the answer,
                    // so only a resolved single-kind one can show as chosen.
                    selected={Boolean(kind) && spec.kinds.length === 1 && spec.kinds[0] === kind}
                    opensList={spec.kinds.length > 1}
                    onPick={() => onPickCategory(id)}
                  />
                );
              })}
        </div>
      </div>

      {/* Only an application is asked for a name HERE. Every managed kind is
          given one the moment it is picked (defaultManagedName) and can edit it
          on its own configuration step; an application has no such step and no
          default worth guessing, since the repository it would be named after
          has not been chosen yet. */}
      {kind === "app" && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="wizard-name">Name</Label>
          <Input
            id="wizard-name"
            value={name}
            onChange={(e) => onNameChange(e.target.value)}
            placeholder="storefront"
            className="font-mono"
            spellCheck={false}
            aria-invalid={nameProblem ? true : undefined}
          />
          <p
            className={cn(
              "text-xs",
              nameProblem ? "text-destructive" : "text-muted-foreground"
            )}
          >
            {nameProblem ?? "Used for the container, its private DNS name and its volumes."}
          </p>
        </div>
      )}

      {availability && !availability.available && (
        <div className="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-amber-600" />
          <p className="min-w-0 text-xs text-muted-foreground">{availability.reason}</p>
        </div>
      )}
    </div>
  );
}

/**
 * One offer on step 1 — a category, or a kind inside one.
 *
 * The same card for both because they are the same decision at two altitudes,
 * and because the contract that matters is identical: an offer that leads
 * nowhere says so in its own words, right here, and carries the single action
 * that fixes it instead of being greyed out and silent.
 *
 * A kind row inside a category passes no `detail`. "Managed PostgreSQL" under a
 * card labelled PostgreSQL, in a list headed Database, is the third time the
 * same word appears — and a blurb per kind would be one more table the control
 * plane does not own.
 */
function PickerCard({
  icon: Icon,
  label,
  detail,
  availability,
  selected,
  opensList = false,
  onPick,
}: {
  icon: React.ElementType;
  label: string;
  detail?: string;
  availability: KindAvailability;
  selected: boolean;
  opensList?: boolean;
  onPick: () => void;
}) {
  const { available, reason, action } = availability;
  const line = available ? detail : reason;
  return (
    <button
      type="button"
      disabled={!available}
      aria-pressed={selected}
      onClick={() => available && onPick()}
      className={cn(
        "flex items-start gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors",
        !available && "cursor-not-allowed border-border bg-muted/40",
        available &&
          (selected
            ? "border-primary bg-primary/5 ring-1 ring-primary/20"
            : "border-border bg-card hover:bg-muted/50")
      )}
    >
      <Icon
        className={cn(
          "mt-0.5 size-4 shrink-0",
          available ? "text-muted-foreground" : "text-muted-foreground/50"
        )}
      />
      <span className="min-w-0 flex-1">
        <span
          className={cn(
            "block text-sm font-medium",
            available ? "text-foreground" : "text-muted-foreground"
          )}
        >
          {label}
        </span>
        {line && (
          <span className="block text-xs leading-snug text-muted-foreground">{line}</span>
        )}
        {!available && action && (
          <Link
            href={action.href}
            className="mt-1 inline-flex items-center gap-1 text-xs font-medium text-primary underline-offset-2 hover:underline"
          >
            {action.label}
            <ExternalLink className="size-3" />
          </Link>
        )}
      </span>
      {selected && <Check className="mt-0.5 size-4 shrink-0 text-primary" />}
      {!selected && available && opensList && (
        <ChevronRight className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
      )}
    </button>
  );
}
