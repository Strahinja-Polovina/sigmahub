"use client";

import * as React from "react";
import { Loader2, Plus } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
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

export function NewEnvironmentDialog({
  projectId,
  projectName,
}: {
  projectId: string;
  projectName: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");
  const [pending, startTransition] = React.useTransition();

  function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    startTransition(async () => {
      try {
        await createEnvironment({ projectId, name: trimmed });
        toast.success(`Environment “${trimmed}” added to ${projectName}`, {
          description: "Attach servers to deploy resources here.",
        });
        setOpen(false);
        setName("");
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
        if (!next) setName("");
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
          <div className="flex flex-col gap-2 py-4">
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
