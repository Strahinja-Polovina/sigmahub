"use client";

import * as React from "react";
import {
  GitBranch,
  GitFork,
  Loader2,
  Plus,
  Rocket,
  Search,
  Trash2,
  Unplug,
} from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  connectRepo,
  detectRepo,
  disconnectRepo,
  promoteBranch,
  removeBranchMapping,
  setBranchMapping,
} from "@/server/actions/git";

// Local mirrors of the CP shapes (kept off the server-only cp module).
type Detected = {
  hasDockerfile: boolean;
  hasCompose: boolean;
  ports: number[];
  env: string[];
  healthCheck?: string;
  deployable: boolean;
  reason?: string;
};
type BranchMap = {
  id: string;
  branch: string;
  environmentId: string;
  policy: "auto" | "manual";
  lastSha?: string;
};
type Connection = { id: string; repoFullName: string };
export type GitConnectionPanel = { connection: Connection; branchMaps: BranchMap[] };
type EnvOption = { id: string; name: string };

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : "Please try again.";
}

/** Detected-config chips shown after a repo scan and after connect. */
function DetectedConfig({ d }: { d: Detected }) {
  return (
    <div className="flex flex-col gap-2 rounded-md border border-border bg-muted/40 p-3 text-sm">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-muted-foreground">Detected:</span>
        {d.hasDockerfile && (
          <span className="rounded bg-card px-1.5 py-0.5 text-xs font-medium">Dockerfile</span>
        )}
        {d.hasCompose && (
          <span className="rounded bg-card px-1.5 py-0.5 text-xs font-medium">Compose</span>
        )}
        {d.healthCheck && (
          <span className="rounded bg-card px-1.5 py-0.5 text-xs font-medium">health check</span>
        )}
      </div>
      <div className="grid gap-1 text-xs text-muted-foreground">
        <span>
          Ports:{" "}
          <span className="font-mono text-foreground">
            {d.ports.length ? d.ports.join(", ") : "—"}
          </span>
        </span>
        <span>
          Env:{" "}
          <span className="font-mono text-foreground">
            {d.env.length ? d.env.join(", ") : "—"}
          </span>
        </span>
      </div>
    </div>
  );
}

function ConnectRepoDialog({
  orgId,
  projectId,
}: {
  orgId: string;
  projectId: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [repo, setRepo] = React.useState("");
  const [token, setToken] = React.useState("");
  const [detected, setDetected] = React.useState<Detected | null>(null);
  const [detecting, setDetecting] = React.useState(false);
  const [pending, startTransition] = React.useTransition();

  function reset() {
    setRepo("");
    setToken("");
    setDetected(null);
  }

  function runDetect() {
    if (!repo.includes("/")) {
      toast.error("Enter the repository as owner/name.");
      return;
    }
    setDetecting(true);
    setDetected(null);
    detectRepo({ orgId, repoFullName: repo.trim(), token: token.trim() || undefined })
      .then((d) => setDetected(d))
      .catch((err) => toast.error("Couldn’t read repository", { description: errMsg(err) }))
      .finally(() => setDetecting(false));
  }

  function connect() {
    startTransition(async () => {
      try {
        await connectRepo({
          orgId,
          projectId,
          repoFullName: repo.trim(),
          token: token.trim() || undefined,
        });
        toast.success(`Connected ${repo.trim()}`);
        setOpen(false);
        reset();
      } catch (err) {
        toast.error("Couldn’t connect repository", { description: errMsg(err) });
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
          <Button size="sm" className="gap-1.5">
            <GitFork className="size-3.5" />
            Connect repo
          </Button>
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Connect a repository</DialogTitle>
          <DialogDescription>
            Enter a GitHub repository. sigmahub scans it for a Dockerfile or Compose file and
            pre-fills the detected ports, environment variables and health check.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4">
          <div className="flex flex-col gap-2">
            <Label htmlFor="git-repo">Repository</Label>
            <div className="flex gap-2">
              <Input
                id="git-repo"
                placeholder="owner/name"
                value={repo}
                onChange={(e) => {
                  setRepo(e.target.value);
                  setDetected(null);
                }}
                autoComplete="off"
              />
              <Button type="button" variant="outline" onClick={runDetect} disabled={detecting}>
                {detecting ? <Loader2 className="size-4 animate-spin" /> : <Search className="size-4" />}
                Detect
              </Button>
            </div>
          </div>

          <div className="flex flex-col gap-2">
            <Label htmlFor="git-token">Access token (private repos)</Label>
            <Input
              id="git-token"
              type="password"
              placeholder="ghp_… (optional for public repos)"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              autoComplete="off"
            />
            <p className="text-xs text-muted-foreground">
              Stored encrypted (per-org envelope) — sigmahub uses it to read the repo and deploy.
            </p>
          </div>

          {detected && !detected.deployable && (
            <p className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">
              {detected.reason ?? "This repository is not deployable."}
            </p>
          )}
          {detected && detected.deployable && <DetectedConfig d={detected} />}
        </div>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
            Cancel
          </DialogClose>
          <Button
            onClick={connect}
            disabled={pending || (detected !== null && !detected.deployable)}
          >
            {pending && <Loader2 className="size-4 animate-spin" />}
            Connect
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Add-a-mapping row: branch text, environment, policy. */
function AddMappingRow({
  orgId,
  projectId,
  connectionId,
  environments,
}: {
  orgId: string;
  projectId: string;
  connectionId: string;
  environments: EnvOption[];
}) {
  const [branch, setBranch] = React.useState("");
  const [envId, setEnvId] = React.useState(environments[0]?.id ?? "");
  const [policy, setPolicy] = React.useState<"auto" | "manual">("auto");
  const [pending, startTransition] = React.useTransition();

  function add() {
    if (!branch.trim()) {
      toast.error("Branch is required.");
      return;
    }
    if (!envId) {
      toast.error("Add an environment first.");
      return;
    }
    startTransition(async () => {
      try {
        await setBranchMapping({ orgId, projectId, connectionId, branch: branch.trim(), environmentId: envId, policy });
        toast.success(`Mapped ${branch.trim()} → ${policy}`);
        setBranch("");
      } catch (err) {
        toast.error("Couldn’t map branch", { description: errMsg(err) });
      }
    });
  }

  return (
    <div className="flex flex-wrap items-end gap-2 pt-3">
      <div className="flex min-w-32 flex-1 flex-col gap-1">
        <Label className="text-xs text-muted-foreground">Branch</Label>
        <Input
          placeholder="main"
          value={branch}
          onChange={(e) => setBranch(e.target.value)}
          className="h-8"
          autoComplete="off"
        />
      </div>
      <div className="flex min-w-32 flex-col gap-1">
        <Label className="text-xs text-muted-foreground">Environment</Label>
        <Select value={envId} onValueChange={(v) => setEnvId(v as string)}>
          <SelectTrigger className="h-8 w-full">
            <SelectValue>
              {(v) => environments.find((e) => e.id === (v as string))?.name ?? "Select"}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {environments.map((e) => (
              <SelectItem key={e.id} value={e.id}>
                {e.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <div className="flex min-w-28 flex-col gap-1">
        <Label className="text-xs text-muted-foreground">Policy</Label>
        <Select value={policy} onValueChange={(v) => setPolicy(v as "auto" | "manual")}>
          <SelectTrigger className="h-8 w-full">
            <SelectValue>{(v) => (v === "auto" ? "Auto-deploy" : "Manual")}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="auto">Auto-deploy</SelectItem>
            <SelectItem value="manual">Manual promote</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <Button size="sm" variant="outline" onClick={add} disabled={pending} className="h-8">
        {pending ? <Loader2 className="size-3.5 animate-spin" /> : <Plus className="size-3.5" />}
        Map
      </Button>
    </div>
  );
}

function ConnectionCard({
  orgId,
  projectId,
  panel,
  environments,
}: {
  orgId: string;
  projectId: string;
  panel: GitConnectionPanel;
  environments: EnvOption[];
}) {
  const [pending, startTransition] = React.useTransition();
  const envName = (id: string) => environments.find((e) => e.id === id)?.name ?? id;

  function promote(mapId: string, branch: string) {
    startTransition(async () => {
      try {
        await promoteBranch({ orgId, projectId, mapId, branch });
        toast.success(`Promoted ${branch}`);
      } catch (err) {
        toast.error("Couldn’t promote", { description: errMsg(err) });
      }
    });
  }

  function unmap(mapId: string) {
    startTransition(async () => {
      try {
        await removeBranchMapping({ orgId, projectId, mapId });
        toast.success("Mapping removed");
      } catch (err) {
        toast.error("Couldn’t remove mapping", { description: errMsg(err) });
      }
    });
  }

  function disconnect() {
    startTransition(async () => {
      try {
        await disconnectRepo({ orgId, projectId, connectionId: panel.connection.id, repoFullName: panel.connection.repoFullName });
        toast.success(`Disconnected ${panel.connection.repoFullName}`);
      } catch (err) {
        toast.error("Couldn’t disconnect", { description: errMsg(err) });
      }
    });
  }

  return (
    <div className="rounded-lg border border-border">
      <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-2.5">
        <span className="inline-flex items-center gap-2 text-sm font-medium text-foreground">
          <GitFork className="size-4 text-muted-foreground" />
          {panel.connection.repoFullName}
        </span>
        <Button
          variant="ghost"
          size="sm"
          className="text-muted-foreground hover:text-destructive"
          onClick={disconnect}
          disabled={pending}
        >
          <Unplug className="size-3.5" />
          Disconnect
        </Button>
      </div>
      <div className="px-4 py-3">
        {panel.branchMaps.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No branch routes yet. Map a branch to an environment to deploy on push.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-0">Branch</TableHead>
                <TableHead>Environment</TableHead>
                <TableHead>Policy</TableHead>
                <TableHead className="pr-0 text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {panel.branchMaps.map((m) => (
                <TableRow key={m.id}>
                  <TableCell className="pl-0 font-mono text-foreground">
                    <span className="inline-flex items-center gap-1.5">
                      <GitBranch className="size-3.5 text-muted-foreground" />
                      {m.branch}
                    </span>
                  </TableCell>
                  <TableCell>{envName(m.environmentId)}</TableCell>
                  <TableCell>
                    <span
                      className={`rounded-full border px-2 py-0.5 text-xs font-medium ${
                        m.policy === "auto"
                          ? "border-emerald-500/30 text-emerald-700"
                          : "border-amber-500/30 text-amber-700"
                      }`}
                    >
                      {m.policy === "auto" ? "Auto-deploy" : "Manual"}
                    </span>
                  </TableCell>
                  <TableCell className="pr-0">
                    <div className="flex items-center justify-end gap-1">
                      {m.policy === "manual" && (
                        <Button
                          variant="outline"
                          size="sm"
                          className="h-7"
                          disabled={pending || !m.lastSha}
                          title={m.lastSha ? "Deploy the last pushed commit" : "No commit pushed yet"}
                          onClick={() => promote(m.id, m.branch)}
                        >
                          <Rocket className="size-3.5" />
                          Promote
                        </Button>
                      )}
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        aria-label={`Remove ${m.branch} mapping`}
                        disabled={pending}
                        onClick={() => unmap(m.id)}
                      >
                        <Trash2 className="size-3.5" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <AddMappingRow
          orgId={orgId}
          projectId={projectId}
          connectionId={panel.connection.id}
          environments={environments}
        />
      </div>
    </div>
  );
}

export function ProjectGitPanel({
  orgId,
  projectId,
  connections,
  environments,
}: {
  orgId: string;
  projectId: string;
  connections: GitConnectionPanel[];
  environments: EnvOption[];
}) {
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-2 border-b">
        <div className="grid gap-1">
          <CardTitle className="flex items-center gap-2 text-sm">
            <GitFork className="size-4 text-muted-foreground" />
            Git deploys
          </CardTitle>
          <CardDescription>
            Connect a repository and map branches to environments. Pushes to an auto-deploy branch
            ship on merge; manual branches wait for a promote.
          </CardDescription>
        </div>
        <ConnectRepoDialog orgId={orgId} projectId={projectId} />
      </CardHeader>
      <CardContent className="flex flex-col gap-4 pt-4">
        {connections.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No repositories connected. Connect one to enable push-to-deploy.
          </p>
        ) : (
          connections.map((panel) => (
            <ConnectionCard
              key={panel.connection.id}
              orgId={orgId}
              projectId={projectId}
              panel={panel}
              environments={environments}
            />
          ))
        )}
      </CardContent>
    </Card>
  );
}
