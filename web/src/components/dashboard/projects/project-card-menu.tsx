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
  envCount = 0,
  resources = [],
}: {
  projectId: string;
  name: string;
  description: string;
  /** Where to go after delete (e.g. from the detail page). Omit on the list. */
  redirectTo?: string;
  /** SIGMA-314: the blast radius, so the delete dialog can itemise it rather
   *  than describe a cascade in the abstract. Defaults keep older call sites
   *  compiling; they get the count-free wording and still have to type the
   *  project name. */
  envCount?: number;
  resources?: { id: string; name: string; kind: string; envName: string }[];
}) {
  const router = useRouter();
  const [renameOpen, setRenameOpen] = React.useState(false);
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [rName, setRName] = React.useState(name);
  const [rDesc, setRDesc] = React.useState(description);
  // SIGMA-314: deleting a project runs the CP's cascadeResourceCleanupTx across
  // every environment in it. Deleting a single resource already requires typing
  // that resource's name; this one destroys all of them and used to need one
  // click on a red button under prose. Same bar as the resource delete now.
  const [typed, setTyped] = React.useState("");
  const [pending, startTransition] = React.useTransition();
  const matches = typed.trim() === name;
  // Every card on the projects list renders one of these, so the input id has
  // to be per-project or the labels point at another card's field.
  const confirmId = `confirm-project-${projectId}`;

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
        setTyped("");
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
          if (!next) setTyped("");
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete “{name}”?</DialogTitle>
            <DialogDescription>
              This permanently removes the project, its {envCount}{" "}
              {envCount === 1 ? "environment" : "environments"} and{" "}
              {resources.length === 0
                ? "no resources"
                : `${resources.length} ${
                    resources.length === 1 ? "resource" : "resources"
                  }`}
              , along with their deployment history. This can’t be undone.
            </DialogDescription>
            {/* The dialog used to stop at "deployment history" and never mention
                backups, while the delete cascaded away the restic repo key —
                leaving the snapshots in the customer's bucket as ciphertext
                nothing could open (SIGMA-170). The key is now retained; say so,
                because the databases themselves still go. */}
            <p className="text-sm text-muted-foreground">
              Database volumes and their offsite snapshots are left in place, and
              each database’s backup encryption key is retained so those
              snapshots can still be opened.
            </p>
          </DialogHeader>

          {resources.length > 0 && (
            <ul className="max-h-48 overflow-y-auto rounded-md border border-border bg-muted/40 p-2 text-sm">
              {resources.map((r) => (
                <li key={r.id} className="flex items-center justify-between gap-3 px-1 py-1">
                  <span className="truncate font-medium text-foreground">
                    {r.envName ? `${r.envName}/` : ""}
                    {r.name}
                  </span>
                  <span className="shrink-0 font-mono text-xs text-muted-foreground">
                    {r.kind}
                  </span>
                </li>
              ))}
            </ul>
          )}

          <div className="flex flex-col gap-1.5">
            <Label htmlFor={confirmId} className="text-xs text-muted-foreground">
              Type <span className="font-mono text-foreground">{name}</span> to confirm
            </Label>
            <Input
              id={confirmId}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
            />
          </div>

          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
              Cancel
            </DialogClose>
            <Button variant="destructive" onClick={confirmDelete} disabled={pending || !matches}>
              {pending && <Loader2 className="size-4 animate-spin" />}
              Delete project
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
