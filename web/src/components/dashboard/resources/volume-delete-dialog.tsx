"use client";

import * as React from "react";
import { toast } from "sonner";
import { KeyRound, Loader2, ShieldAlert, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
import { requestVolumeDeleteConfirm, confirmVolumeDelete } from "@/server/actions/resources";

// VolumeDeleteDialog runs the two-phase destructive-op approval for deleting a
// named data volume: it requests a short-lived confirm token from the control
// plane, presents it, and only executes on an explicit second click. Deleting a
// volume is irreversible, so the token step forces a deliberate confirmation.
export function VolumeDeleteDialog({
  resourceId,
  resourceName,
}: {
  resourceId: string;
  resourceName: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [volumeName, setVolumeName] = React.useState("");
  const [issued, setIssued] = React.useState<{ token: string; expiresAt: string } | null>(null);
  const [pending, startTransition] = React.useTransition();

  function reset() {
    setVolumeName("");
    setIssued(null);
  }

  function request() {
    if (!volumeName.trim()) {
      toast.error("Enter the volume name to delete.");
      return;
    }
    startTransition(async () => {
      try {
        const res = await requestVolumeDeleteConfirm({ resourceId, volumeName: volumeName.trim() });
        setIssued({ token: res.token, expiresAt: res.expiresAt });
      } catch (err) {
        toast.error("Couldn’t request confirmation", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  function approve() {
    if (!issued) return;
    startTransition(async () => {
      try {
        await confirmVolumeDelete({ resourceId, volumeName: volumeName.trim(), token: issued.token });
        toast.success(`Deletion of volume “${volumeName.trim()}” approved`);
        setOpen(false);
        reset();
      } catch (err) {
        toast.error("Couldn’t delete the volume", {
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
            <Trash2 className="size-4" />
            Delete a data volume
          </Button>
        }
      />
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <ShieldAlert className="size-5 text-destructive" />
            Delete a data volume
          </DialogTitle>
          <DialogDescription>
            Removing a named volume for <span className="font-medium">{resourceName}</span> permanently
            destroys its data. This needs a confirmation token and an explicit approval.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="volume-name">Volume name</Label>
            <Input
              id="volume-name"
              placeholder="data"
              value={volumeName}
              disabled={pending || issued !== null}
              onChange={(e) => setVolumeName(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              The logical volume name as declared in the resource spec.
            </p>
          </div>

          {issued && (
            <div className="rounded-md border border-border bg-muted/40 p-3 text-sm">
              <div className="flex items-center gap-2 font-medium">
                <KeyRound className="size-4" />
                Confirmation token issued
              </div>
              <p className="mt-1 break-all font-mono text-xs text-muted-foreground">{issued.token}</p>
              <p className="mt-1 text-xs text-muted-foreground">
                Expires {new Date(issued.expiresAt).toLocaleTimeString()}. Approve below to execute.
              </p>
            </div>
          )}
        </div>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>Cancel</DialogClose>
          {issued ? (
            <Button variant="destructive" onClick={approve} disabled={pending}>
              {pending && <Loader2 className="size-4 animate-spin" />}
              Approve &amp; delete
            </Button>
          ) : (
            <Button variant="destructive" onClick={request} disabled={pending || !volumeName.trim()}>
              {pending && <Loader2 className="size-4 animate-spin" />}
              Request confirmation
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
