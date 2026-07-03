"use client";

import * as React from "react";
import { Plus } from "lucide-react";
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

// Mock "New environment" dialog — free-text env name, toast on create.
export function NewEnvironmentDialog({ projectName }: { projectName: string }) {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");

  function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    toast.success(`Environment “${trimmed}” added to ${projectName}`, {
      description: "Attach servers to deploy resources here.",
    });
    setOpen(false);
    setName("");
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
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
            <DialogClose render={<Button variant="outline" type="button" />}>
              Cancel
            </DialogClose>
            <Button type="submit" disabled={!name.trim()}>
              Create environment
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
