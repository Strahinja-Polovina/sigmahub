"use client";

import * as React from "react";
import { Loader2, Plus } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { createEnvironment } from "@/server/actions/projects";

/** Mirrors the server-side isProductionName heuristic — a CONVENIENCE default
 *  for the checkbox only; the user's explicit choice is what's submitted
 *  (SIGMA-190 — the name match used to silently decide backup retention). */
function looksLikeProduction(name: string) {
  const n = name.trim().toLowerCase();
  return n === "production" || n === "prod";
}

export function NewEnvironmentDialog({
  projectId,
  projectName,
}: {
  projectId: string;
  projectName: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");
  const [production, setProduction] = React.useState(false);
  // Tracks whether the user touched the checkbox; until then it follows the
  // name heuristic so "production"/"prod" stay pre-checked.
  const [prodTouched, setProdTouched] = React.useState(false);
  const [pending, startTransition] = React.useTransition();

  const effectiveProduction = prodTouched ? production : looksLikeProduction(name);

  function reset() {
    setName("");
    setProduction(false);
    setProdTouched(false);
  }

  function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    startTransition(async () => {
      try {
        await createEnvironment({ projectId, name: trimmed, production: effectiveProduction });
        toast.success(`Environment “${trimmed}” added to ${projectName}`, {
          description: effectiveProduction
            ? "Production environment — databases here keep 30 daily backups."
            : "Attach servers to deploy resources here.",
        });
        setOpen(false);
        reset();
      } catch (err) {
        toast.error("Couldn’t add environment", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (pending) return;
        setOpen(next);
        if (!next) reset();
      }}
    >
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <Plus />
            New environment
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={handleCreate}>
          <DialogHeader>
            <DialogTitle>New environment</DialogTitle>
            <DialogDescription>
              Environments isolate deployments — e.g. production, staging, dev.
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-4 py-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="environment-name">Name</Label>
              <Input
                id="environment-name"
                placeholder="e.g. staging"
                value={name}
                onChange={(e) => setName(e.target.value)}
                autoFocus
                required
              />
            </div>
            {/* Explicit choice (SIGMA-190): this used to be silently inferred
                from an exact name match, so a prod env named "live" got the
                non-production backup defaults with no indication anywhere. */}
            <label className="flex items-start gap-2.5" htmlFor="environment-production">
              <Checkbox
                id="environment-production"
                checked={effectiveProduction}
                onCheckedChange={(v) => {
                  setProdTouched(true);
                  setProduction(Boolean(v));
                }}
              />
              <span className="flex flex-col gap-0.5">
                <span className="text-sm font-medium text-foreground">Production environment</span>
                <span className="text-xs text-muted-foreground">
                  Databases created here keep 30 daily backups instead of the 7/4/6
                  daily/weekly/monthly default. You can change this later.
                </span>
              </span>
            </label>
          </div>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
              Cancel
            </DialogClose>
            <Button type="submit" disabled={!name.trim() || pending}>
              {pending && <Loader2 className="size-4 animate-spin" />}
              Create environment
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
