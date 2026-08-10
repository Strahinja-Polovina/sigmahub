"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  Eye,
  EyeOff,
  Copy,
  KeyRound,
  Lock,
  Pencil,
  Plus,
  Trash2,
  Loader2,
  ShieldAlert,
} from "lucide-react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
  DialogClose,
} from "@/components/ui/dialog";
import {
  createSecretAction,
  revealSecretAction,
  updateSecretAction,
  deleteSecretAction,
} from "@/server/actions/secrets";
import { createSubmissionId } from "@/lib/request-id";

type SecretScope = "project" | "environment";

type SecretRow = {
  id: string;
  name: string;
  envVar: boolean;
  scope: SecretScope;
};

const MASK = "•".repeat(16);

export function EnvVarsTable({
  resourceId,
  envName,
  secrets,
  canManage,
}: {
  resourceId: string;
  envName: string;
  secrets: SecretRow[];
  canManage: boolean;
}) {
  const router = useRouter();
  // Revealed plaintext values, keyed by secret id. A value only ever lives here
  // after an explicit, audited reveal — never in the props.
  const [values, setValues] = React.useState<Record<string, string>>({});
  const [revealing, setRevealing] = React.useState<string | null>(null);
  const [confirmReveal, setConfirmReveal] = React.useState<SecretRow | null>(null);
  const [deleting, setDeleting] = React.useState<SecretRow | null>(null);
  const [editing, setEditing] = React.useState<SecretRow | null>(null);
  const [createOpen, setCreateOpen] = React.useState(false);

  async function doReveal(secret: SecretRow) {
    setRevealing(secret.id);
    try {
      const { value } = await revealSecretAction({ resourceId, secretId: secret.id });
      setValues((v) => ({ ...v, [secret.id]: value }));
    } catch (err) {
      toast.error("Couldn’t reveal secret", {
        description: err instanceof Error ? err.message : "Please try again.",
      });
    } finally {
      setRevealing(null);
      setConfirmReveal(null);
    }
  }

  function hide(id: string) {
    setValues((v) => {
      const next = { ...v };
      delete next[id];
      return next;
    });
  }

  async function copyValue(secret: SecretRow) {
    try {
      const value =
        values[secret.id] ??
        (await revealSecretAction({ resourceId, secretId: secret.id })).value;
      await navigator.clipboard.writeText(value);
      toast.success(`Copied ${secret.name}`);
    } catch (err) {
      toast.error("Couldn’t copy", {
        description: err instanceof Error ? err.message : "Please try again.",
      });
    }
  }

  async function doDelete(secret: SecretRow) {
    try {
      await deleteSecretAction({ resourceId, secretId: secret.id });
      toast.success(`Deleted ${secret.name}`);
      router.refresh();
    } catch (err) {
      toast.error("Couldn’t delete", {
        description: err instanceof Error ? err.message : "Please try again.",
      });
    } finally {
      setDeleting(null);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center justify-between gap-4">
        <p className="text-sm text-muted-foreground">
          {canManage
            ? "Secrets are encrypted at rest and injected at container start. File-mode secrets land in an in-memory tmpfs; env-var mode is opt-in."
            : "Values are hidden. Ask a Project Admin to reveal or manage secrets."}
        </p>
        {canManage && (
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="size-4" />
            New secret
          </Button>
        )}
      </div>

      <div className="overflow-hidden rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-4">Key</TableHead>
              <TableHead>Value</TableHead>
              <TableHead className="w-40 pr-4 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {secrets.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="py-8 text-center text-sm text-muted-foreground">
                  No secrets yet.
                  {canManage ? " Adding one redeploys the app with it injected." : ""}
                </TableCell>
              </TableRow>
            )}
            {secrets.map((s) => {
              const revealed = s.id in values;
              return (
                <TableRow key={s.id}>
                  <TableCell className="pl-4">
                    <span className="inline-flex items-center gap-2 font-mono text-sm text-foreground">
                      {s.envVar ? (
                        <KeyRound className="size-3.5 text-muted-foreground" />
                      ) : (
                        <Lock className="size-3.5 text-muted-foreground" />
                      )}
                      {s.name}
                      <Badge variant="outline" className="text-[10px]">
                        {s.scope === "environment" ? envName : "project"}
                      </Badge>
                      {s.envVar && (
                        <Badge variant="outline" className="text-[10px]">
                          env var
                        </Badge>
                      )}
                    </span>
                  </TableCell>
                  <TableCell>
                    <span
                      className={
                        revealed
                          ? "font-mono text-sm break-all text-foreground"
                          : "font-mono text-sm tracking-wider text-muted-foreground"
                      }
                    >
                      {revealed ? values[s.id] : MASK}
                    </span>
                  </TableCell>
                  <TableCell className="pr-4 text-right">
                    <div className="inline-flex items-center gap-0.5">
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        disabled={!canManage || revealing === s.id}
                        aria-label={revealed ? "Hide value" : "Reveal value"}
                        title={canManage ? undefined : "Project Admin only"}
                        onClick={() => (revealed ? hide(s.id) : setConfirmReveal(s))}
                      >
                        {revealing === s.id ? (
                          <Loader2 className="size-4 animate-spin text-muted-foreground" />
                        ) : revealed ? (
                          <EyeOff className="size-4 text-muted-foreground" />
                        ) : (
                          <Eye className="size-4 text-muted-foreground" />
                        )}
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        disabled={!canManage}
                        aria-label="Copy value"
                        onClick={() => copyValue(s)}
                      >
                        <Copy className="size-4 text-muted-foreground" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        disabled={!canManage}
                        aria-label="Edit value"
                        onClick={() => setEditing(s)}
                      >
                        <Pencil className="size-4 text-muted-foreground" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        disabled={!canManage}
                        aria-label="Delete secret"
                        onClick={() => setDeleting(s)}
                      >
                        <Trash2 className="size-4 text-muted-foreground" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>

      {/* Reveal confirmation — a deliberate, audited action. */}
      <Dialog open={confirmReveal !== null} onOpenChange={(o) => !o && setConfirmReveal(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Reveal secret value?</DialogTitle>
            <DialogDescription>
              You&apos;re about to display the plaintext value of{" "}
              <span className="font-mono text-foreground">{confirmReveal?.name}</span>. This read is
              recorded in the audit log. Make sure no one is looking over your shoulder.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
            <Button
              disabled={revealing !== null}
              onClick={() => confirmReveal && doReveal(confirmReveal)}
            >
              {revealing ? <Loader2 className="size-4 animate-spin" /> : <Eye className="size-4" />}
              Reveal
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete confirmation. The redeploy warning is the point: a delete mints
          config deployments for EVERY app in the secret's scope, and they come
          back up without the variable. If the operator is rotating a value they
          want Edit, not Delete (SIGMA-264). */}
      <Dialog open={deleting !== null} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete secret?</DialogTitle>
            <DialogDescription>
              <span className="font-mono text-foreground">{deleting?.name}</span> will be removed
              and the app redeployed without it. This can&apos;t be undone.
            </DialogDescription>
          </DialogHeader>
          <Alert variant="destructive">
            <ShieldAlert className="size-4" />
            <AlertTitle>This redeploys every app that uses it</AlertTitle>
            <AlertDescription>
              Deleting restarts{" "}
              {deleting?.scope === "environment"
                ? `every app in ${envName}`
                : "every app in this project"}{" "}
              without <span className="font-mono">{deleting?.name}</span>. To change the value
              instead, close this and use Edit — that keeps the variable in place through a single
              redeploy.
            </AlertDescription>
          </Alert>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
            <Button variant="destructive" onClick={() => deleting && doDelete(deleting)}>
              <Trash2 className="size-4" />
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {canManage && (
        <EditSecretDialog
          secret={editing}
          onOpenChange={(o) => !o && setEditing(null)}
          resourceId={resourceId}
          envName={envName}
          onSaved={() => {
            // A rotated value invalidates whatever this table has revealed.
            if (editing) hide(editing.id);
            setEditing(null);
            router.refresh();
          }}
        />
      )}

      {canManage && (
        <NewSecretDialog
          open={createOpen}
          onOpenChange={setCreateOpen}
          resourceId={resourceId}
          envName={envName}
          onCreated={() => router.refresh()}
        />
      )}
    </div>
  );
}

/** Rotate one secret's value in place (SIGMA-264).
 *
 *  The alternative the product used to force — delete, then create — costs two
 *  rounds of config deployments, and the first of them re-rolls every dependent
 *  app WITHOUT the variable. That is a self-inflicted outage during routine
 *  credential rotation, so the value change has to be a single write that keeps
 *  the secret's id, name and scope.
 *
 *  The current value is deliberately NOT pre-filled: reading it is an audited
 *  action (see the reveal dialog) and rotation doesn't need it. */
function EditSecretDialog({
  secret,
  onOpenChange,
  resourceId,
  envName,
  onSaved,
}: {
  secret: SecretRow | null;
  onOpenChange: (o: boolean) => void;
  resourceId: string;
  envName: string;
  onSaved: () => void;
}) {
  const [value, setValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);

  async function submit() {
    if (!secret) return;
    setBusy(true);
    try {
      await updateSecretAction({ resourceId, secretId: secret.id, value });
      toast.success(`Updated ${secret.name}`, {
        description: "Apps using it are redeploying with the new value.",
      });
      // Clear the draft on the way out as well as on cancel: the dialog is
      // controlled by the parent's selection, so a saved value left behind here
      // would show up pre-filled the next time ANOTHER secret is opened.
      setValue("");
      onSaved();
    } catch (err) {
      toast.error("Couldn’t update secret", {
        description: err instanceof Error ? err.message : "Please try again.",
      });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open={secret !== null}
      onOpenChange={(o) => {
        if (!o) setValue("");
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit value</DialogTitle>
          <DialogDescription>
            Replace the value of <span className="font-mono text-foreground">{secret?.name}</span>.
            The key, its {secret?.scope === "environment" ? envName : "project"} scope and its
            injection mode stay as they are, and every app using it redeploys once with the new
            value.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-2 py-2">
          <Label htmlFor="secret-new-value">New value</Label>
          <Textarea
            id="secret-new-value"
            placeholder="Paste the rotated credential…"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            className="font-mono min-h-20"
            autoComplete="off"
            spellCheck={false}
          />
        </div>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
          <Button disabled={busy || value.length === 0} onClick={submit}>
            {busy ? <Loader2 className="size-4 animate-spin" /> : <Pencil className="size-4" />}
            Save value
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function NewSecretDialog({
  open,
  onOpenChange,
  resourceId,
  envName,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  resourceId: string;
  envName: string;
  onCreated: () => void;
}) {
  const [name, setName] = React.useState("");
  const [value, setValue] = React.useState("");
  const [scope, setScope] = React.useState<SecretScope>("environment");
  const [envVar, setEnvVar] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  // One request id per SUBMISSION (SIGMA-256). Pressing Save again after an
  // apparent failure is a retry and reuses it, so a create the control plane
  // already committed — and whose response died at the proxy — is replayed
  // rather than executed a second time. Editing a field, or saving a second
  // secret, is a new intent and gets a new id.
  const submission = React.useRef(createSubmissionId());

  function reset() {
    setName("");
    setValue("");
    setScope("environment");
    setEnvVar(false);
  }

  async function submit() {
    setBusy(true);
    const requestId = submission.current.forContent(
      JSON.stringify([resourceId, name, value, scope, envVar])
    );
    try {
      await createSecretAction({ resourceId, name, value, scope, envVar, requestId });
      submission.current.settled();
      toast.success(`Secret ${name} created`);
      reset();
      onOpenChange(false);
      onCreated();
    } catch (err) {
      toast.error("Couldn’t create secret", {
        description: err instanceof Error ? err.message : "Please try again.",
      });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) reset();
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New secret</DialogTitle>
          <DialogDescription>
            Stored encrypted under your organization&apos;s key and injected when the container
            starts.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 py-2">
          <div className="flex flex-col gap-2">
            <Label htmlFor="secret-name">Key</Label>
            <Input
              id="secret-name"
              placeholder="DATABASE_URL"
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="font-mono"
              autoComplete="off"
              spellCheck={false}
            />
          </div>

          <div className="flex flex-col gap-2">
            <Label htmlFor="secret-value">Value</Label>
            <Textarea
              id="secret-value"
              placeholder="postgres://…"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              className="font-mono min-h-20"
              autoComplete="off"
              spellCheck={false}
            />
          </div>

          <div className="flex flex-col gap-2">
            <Label htmlFor="secret-scope">Scope</Label>
            <Select value={scope} onValueChange={(v) => setScope(v as SecretScope)}>
              <SelectTrigger id="secret-scope">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="environment">
                  Environment — {envName} (this resource only)
                </SelectItem>
                <SelectItem value="project">Project — shared by every environment</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="flex items-start justify-between gap-4 rounded-lg border border-border p-3">
            <div className="flex flex-col gap-1">
              <Label htmlFor="secret-envvar" className="cursor-pointer">
                Inject as environment variable
              </Label>
              <p className="text-xs text-muted-foreground">
                Off (default): mounted as an in-memory tmpfs file, never on disk. On: exported into
                the process environment.
              </p>
            </div>
            <Switch id="secret-envvar" checked={envVar} onCheckedChange={setEnvVar} />
          </div>

          {envVar && (
            <Alert variant="destructive">
              <ShieldAlert className="size-4" />
              <AlertTitle>Environment variables are visible on the host</AlertTitle>
              <AlertDescription>
                An env-var secret appears in <span className="font-mono">docker inspect</span> and
                the container config on the host&apos;s disk. Only enable this on a host with disk
                encryption. Prefer file mode for anything sensitive.
              </AlertDescription>
            </Alert>
          )}
        </div>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
          <Button disabled={busy || !name.trim() || value.length === 0} onClick={submit}>
            {busy ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
            Create secret
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
