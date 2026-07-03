"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { Loader2, MoreHorizontal, Pencil, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { deleteProject, renameProject } from "@/server/actions/projects";

const msg = (err: unknown) =>
  err instanceof Error ? err.message : "Please try again.";

export function ProjectCardMenu({
  projectId,
  name,
  description,
  redirectTo,
}: {
  projectId: string;
  name: string;
  description: string;
  /** Where to go after delete (e.g. from the detail page). Omit on the list. */
  redirectTo?: string;
}) {
  const router = useRouter();
  const [renameOpen, setRenameOpen] = React.useState(false);
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [rName, setRName] = React.useState(name);
  const [rDesc, setRDesc] = React.useState(description);
  const [pending, startTransition] = React.useTransition();

  function submitRename(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = rName.trim();
    if (!trimmed) return;
    startTransition(async () => {
      try {
        await renameProject({ projectId, name: trimmed, description: rDesc });
        toast.success("Project updated");
        setRenameOpen(false);
      } catch (err) {
        toast.error("Couldn’t update project", { description: msg(err) });
      }
    });
  }

  function confirmDelete() {
    startTransition(async () => {
      try {
        await deleteProject({ projectId });
        toast.success(`Project “${name}” deleted`);
        setDeleteOpen(false);
        if (redirectTo) router.push(redirectTo);
      } catch (err) {
        toast.error("Couldn’t delete project", { description: msg(err) });
      }
    });
  }

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              variant="ghost"
              size="icon"
              aria-label="Project actions"
              className="relative z-10 size-7 text-muted-foreground"
            >
              <MoreHorizontal className="size-4" />
            </Button>
          }
        />
        <DropdownMenuContent align="end" side="bottom" sideOffset={4} className="w-40">
          <DropdownMenuItem
            className="gap-2"
            onClick={() => {
              setRName(name);
              setRDesc(description);
              setRenameOpen(true);
            }}
          >
            <Pencil className="size-4 text-muted-foreground" />
            Rename
          </DropdownMenuItem>
          <DropdownMenuItem
            variant="destructive"
            className="gap-2"
            onClick={() => setDeleteOpen(true)}
          >
            <Trash2 className="size-4" />
            Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      {/* Rename */}
      <Dialog
        open={renameOpen}
        onOpenChange={(next) => {
          if (pending) return;
          setRenameOpen(next);
        }}
      >
        <DialogContent>
          <form onSubmit={submitRename}>
            <DialogHeader>
              <DialogTitle>Rename project</DialogTitle>
              <DialogDescription>Update the project name and description.</DialogDescription>
            </DialogHeader>
            <div className="flex flex-col gap-4 py-4">
              <div className="flex flex-col gap-2">
                <Label htmlFor="rename-project-name">Name</Label>
                <Input
                  id="rename-project-name"
                  value={rName}
                  onChange={(e) => setRName(e.target.value)}
                  autoFocus
                  required
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="rename-project-description">Description</Label>
                <Textarea
                  id="rename-project-description"
                  value={rDesc}
                  onChange={(e) => setRDesc(e.target.value)}
                  rows={3}
                />
              </div>
            </div>
            <DialogFooter>
              <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
                Cancel
              </DialogClose>
              <Button type="submit" disabled={!rName.trim() || pending}>
                {pending && <Loader2 className="size-4 animate-spin" />}
                Save changes
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Delete confirm */}
      <Dialog
        open={deleteOpen}
        onOpenChange={(next) => {
          if (pending) return;
          setDeleteOpen(next);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete “{name}”?</DialogTitle>
            <DialogDescription>
              This permanently removes the project and all of its environments,
              resources and deployment history. This can’t be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
              Cancel
            </DialogClose>
            <Button variant="destructive" onClick={confirmDelete} disabled={pending}>
              {pending && <Loader2 className="size-4 animate-spin" />}
              Delete project
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
