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
  deleteSecretAction,
} from "@/server/actions/secrets";

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
              <TableHead className="w-32 pr-4 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {secrets.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="py-8 text-center text-sm text-muted-foreground">
                  No secrets yet.
                  {canManage ? " Add one to inject it at the next deploy." : ""}
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

      {/* Delete confirmation. */}
      <Dialog open={deleting !== null} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete secret?</DialogTitle>
            <DialogDescription>
              <span className="font-mono text-foreground">{deleting?.name}</span> will be removed
              and no longer injected on the next deploy. This can&apos;t be undone.
            </DialogDescription>
          </DialogHeader>
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

  function reset() {
    setName("");
    setValue("");
    setScope("environment");
    setEnvVar(false);
  }

  async function submit() {
    setBusy(true);
    try {
      await createSecretAction({ resourceId, name, value, scope, envVar });
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
