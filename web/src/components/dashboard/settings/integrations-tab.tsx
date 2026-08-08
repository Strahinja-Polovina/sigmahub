"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  FolderGit2,
  Loader2,
  Plug,
  Unplug,
  ExternalLink,
  AlertTriangle,
} from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { githubInstallUrl } from "@/lib/github-app";
import { disconnectGitIntegration } from "@/server/actions/git";
import { RegistryCard } from "./registry-card";
import type { CpImageRegistry } from "@/server/cp";

export type Installation = {
  installationId: string;
  accountLogin: string;
  accountType: string;
  createdBy: string;
  createdAt: string;
};

/**
 * Org-level integrations. GitHub is connected once here and every project then
 * PICKS repositories from it — the old flow made you connect each repo by hand
 * (and paste a token) before it could deploy.
 */
export function IntegrationsTab({
  orgId,
  enabled,
  slug,
  installations,
  canManage,
  registry = { configured: false, registry: null, repository: "" },
}: {
  orgId: string;
  /** The control plane has a GitHub App registered at all. */
  enabled: boolean;
  slug: string;
  installations: Installation[];
  canManage: boolean;
  /** The org's container registry — where built images go so a machine other
   *  than the builder can run them. */
  registry?: {
    configured: boolean;
    registry: CpImageRegistry | null;
    repository: string;
  };
}) {
  const connected = installations.length > 0;

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader className="border-b">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="flex flex-col gap-1.5">
              <CardTitle className="inline-flex items-center gap-2">
                <FolderGit2 className="size-4" />
                GitHub
              </CardTitle>
              <CardDescription>
                Connect once for the whole organization. Every project then picks
                repositories from a list — no per-repo setup, no access tokens to paste.
              </CardDescription>
            </div>
            {connected ? (
              <Badge variant="outline" className="shrink-0 gap-1.5">
                <Plug className="size-3" />
                Connected
              </Badge>
            ) : (
              <Badge variant="outline" className="shrink-0 text-muted-foreground">
                Not connected
              </Badge>
            )}
          </div>
        </CardHeader>

        <CardContent className="flex flex-col gap-4 pt-4">
          {!enabled ? (
            <Alert>
              <AlertTriangle className="size-4" />
              <AlertTitle>No GitHub App is registered on this control plane</AlertTitle>
              <AlertDescription>
                An operator needs to register a GitHub App and set{" "}
                <span className="font-mono text-xs">CP_GITHUB_APP_ID</span>,{" "}
                <span className="font-mono text-xs">CP_GITHUB_APP_PRIVATE_KEY</span> and{" "}
                <span className="font-mono text-xs">CP_GITHUB_APP_SLUG</span>. Until then,
                repositories can still be connected one at a time with an access token.
              </AlertDescription>
            </Alert>
          ) : connected ? (
            <ul className="flex flex-col divide-y divide-border">
              {installations.map((inst) => (
                <li
                  key={inst.installationId}
                  className="flex flex-wrap items-center justify-between gap-3 py-3 first:pt-0 last:pb-0"
                >
                  <div className="flex min-w-0 flex-col gap-0.5">
                    <span className="truncate text-sm font-medium text-foreground">
                      {inst.accountLogin || `Installation ${inst.installationId}`}
                      {inst.accountType && (
                        <span className="ml-2 text-xs font-normal text-muted-foreground">
                          {inst.accountType}
                        </span>
                      )}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      Connected by {inst.createdBy || "an admin"} ·{" "}
                      {new Date(inst.createdAt).toLocaleDateString("en-GB", {
                        day: "numeric",
                        month: "short",
                        year: "numeric",
                      })}
                    </span>
                  </div>
                  {canManage && (
                    <DisconnectButton orgId={orgId} installation={inst} />
                  )}
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">
              Install the SigmaHub GitHub App on your account or organization. You choose
              which repositories it can read — SigmaHub never asks for more than you grant.
            </p>
          )}

          {enabled && canManage && (
            <div className="flex flex-wrap items-center gap-2">
              <Button
                size="sm"
                variant={connected ? "outline" : "default"}
                render={<a href={githubInstallUrl(slug, { kind: "org" })} />}
              >
                <FolderGit2 className="size-4" />
                {connected ? "Add another account" : "Connect GitHub"}
                <ExternalLink className="size-3.5" />
              </Button>
              {connected && (
                <span className="text-xs text-muted-foreground">
                  Changing which repositories are shared is done on GitHub.
                </span>
              )}
            </div>
          )}

          {enabled && !canManage && (
            <p className="text-xs text-muted-foreground">
              Only organization admins can change integrations.
            </p>
          )}
        </CardContent>
      </Card>

      <RegistryCard
        orgId={orgId}
        configured={registry.configured}
        registry={registry.registry}
        repository={registry.repository}
        canManage={canManage}
      />
    </div>
  );
}

/** Disconnect, with the blast radius spelled out when repos still use it. */
function DisconnectButton({
  orgId,
  installation,
}: {
  orgId: string;
  installation: Installation;
}) {
  const router = useRouter();
  const [pending, startTransition] = React.useTransition();
  const [inUse, setInUse] = React.useState<number | null>(null);

  function disconnect(force: boolean) {
    startTransition(async () => {
      try {
        await disconnectGitIntegration({
          orgId,
          installationId: installation.installationId,
          force,
        });
        toast.success("GitHub disconnected");
        setInUse(null);
        router.refresh();
      } catch (err) {
        const message = err instanceof Error ? err.message : "Please try again.";
        // The CP answers 409 with how many repos would stop deploying; turn that
        // into a confirmation rather than a dead-end error.
        const match = /"connections":\s*(\d+)/.exec(message);
        if (match) {
          setInUse(Number(match[1]));
          return;
        }
        toast.error("Couldn’t disconnect", { description: message });
      }
    });
  }

  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        className="shrink-0"
        disabled={pending}
        onClick={() => disconnect(false)}
      >
        {pending ? <Loader2 className="size-4 animate-spin" /> : <Unplug className="size-4" />}
        Disconnect
      </Button>

      <Dialog open={inUse !== null} onOpenChange={(o) => !o && setInUse(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Disconnect GitHub anyway?</DialogTitle>
            <DialogDescription>
              {inUse} {inUse === 1 ? "repository" : "repositories"} still deploy through{" "}
              <span className="font-medium text-foreground">
                {installation.accountLogin || installation.installationId}
              </span>
              . Disconnecting stops push-to-deploy for {inUse === 1 ? "it" : "them"} —
              already-running containers keep running, but new pushes will be ignored
              until you reconnect.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
            <Button variant="destructive" disabled={pending} onClick={() => disconnect(true)}>
              {pending ? <Loader2 className="size-4 animate-spin" /> : <Unplug className="size-4" />}
              Disconnect anyway
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
