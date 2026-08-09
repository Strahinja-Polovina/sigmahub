"use client";

import * as React from "react";
import { GitBranch, FolderGit2, Loader2, Search, CircleAlert, ChevronRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { githubInstallUrl } from "@/lib/github-app";
import { RepoPicker } from "../repo-picker";
import { searchMockRepos, type MockRepo } from "@/lib/mock/repos";

/**
 * Pick a repository — GitHub App first (SIGMA-208).
 *
 * The old step led with an owner/repo text field and a personal access token,
 * and mentioned the org integration in a paragraph of grey text at the bottom.
 * That is backwards: the App is the supported path, the token is the escape
 * hatch for a public repo or an org that will not install it. Worse, when the
 * integration was missing the only offer was "go to Settings › Integrations" —
 * a link out of the wizard, which discards everything chosen so far.
 *
 * So: the picker is what you see. If there is no integration, the PRIMARY
 * action is connecting one from here, and the return trip lands back in this
 * wizard with the draft intact.
 */
export function SourceStep({
  cpMode,
  orgId,
  repo,
  branch,
  onPickRepo,
  onBranchChange,
  detecting,
  gitAppSlug,
  installUrlTarget,
  onBeforeLeaveForGitHub,
  manualRepo,
  onManualRepoChange,
  token,
  onTokenChange,
  onDetectManual,
  detectError,
}: {
  cpMode: boolean;
  orgId: string;
  repo: { fullName: string; defaultBranch: string; branches?: string[] } | null;
  branch: string;
  onPickRepo: (repo: {
    fullName: string;
    defaultBranch: string;
    installationId?: string;
    branches?: string[];
    mock?: MockRepo;
  }) => void;
  onBranchChange: (branch: string) => void;
  detecting: boolean;
  /** GitHub App slug, when this control plane has one configured. */
  gitAppSlug: string | null;
  installUrlTarget: { kind: "wizard"; projectId?: string };
  /** Stash the draft before navigating to github.com. */
  onBeforeLeaveForGitHub: () => void;
  manualRepo: string;
  onManualRepoChange: (value: string) => void;
  token: string;
  onTokenChange: (value: string) => void;
  onDetectManual: () => void;
  detectError: string | null;
}) {
  // `pickerEmpty` flips when the org has no usable integration. It drives the
  // Connect-GitHub offer, not a silent fall-through to the token form.
  const [pickerEmpty, setPickerEmpty] = React.useState(false);
  // The manual path is opt-in. It exists, it is reachable in one click, and it
  // is not what a first-time user is asked to do.
  const [manualOpen, setManualOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");

  const mockResults = React.useMemo(() => (cpMode ? [] : searchMockRepos(query)), [cpMode, query]);

  return (
    <div className="flex flex-col gap-4">
      {cpMode && !pickerEmpty && (
        <RepoPicker
          orgId={orgId}
          value={repo?.fullName ?? null}
          onUnavailable={() => setPickerEmpty(true)}
          onSelect={(picked) =>
            onPickRepo({
              fullName: picked.fullName,
              defaultBranch: picked.defaultBranch || "main",
              installationId: picked.installationId,
            })
          }
        />
      )}

      {cpMode && pickerEmpty && (
        <div className="flex flex-col gap-3 rounded-lg border border-primary/30 bg-primary/5 p-4">
          <div className="flex items-start gap-3">
            <FolderGit2 className="mt-0.5 size-5 shrink-0 text-foreground" />
            <div className="min-w-0 flex-1">
              <p className="text-sm font-medium text-foreground">Connect GitHub</p>
              <p className="text-xs text-muted-foreground">
                Install the sigmahub app once and every repository it grants shows up
                here to pick from — no tokens to mint, rotate or leak. You come back
                to this wizard with everything you&apos;ve chosen so far.
              </p>
            </div>
          </div>
          {gitAppSlug ? (
            <Button
              size="sm"
              className="w-fit"
              render={
                <a
                  href={githubInstallUrl(gitAppSlug, installUrlTarget)}
                  onClick={onBeforeLeaveForGitHub}
                />
              }
            >
              <FolderGit2 className="size-4" />
              Connect GitHub
            </Button>
          ) : (
            <p className="text-xs text-muted-foreground">
              This control plane has no GitHub App configured, so the token path below
              is the only way in.
            </p>
          )}
        </div>
      )}

      {/* Demo mode: the picker's data is local, so the same shape is presented
          rather than a different screen — a demo that looks nothing like the
          product is a demo of nothing. */}
      {!cpMode && (
        <div className="flex flex-col gap-2">
          <div className="relative">
            <Search className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Filter repositories"
              className="pl-8 font-mono"
              aria-label="Filter repositories"
            />
          </div>
          <div className="max-h-56 overflow-y-auto rounded-lg border border-border">
            {mockResults.length === 0 ? (
              <p className="p-4 text-center text-sm text-muted-foreground">
                No repository matches “{query}”.
              </p>
            ) : (
              <ul className="divide-y divide-border">
                {mockResults.map((r) => {
                  const selected = repo?.fullName === r.fullName;
                  return (
                    <li key={r.fullName}>
                      <button
                        type="button"
                        aria-pressed={selected}
                        onClick={() =>
                          onPickRepo({
                            fullName: r.fullName,
                            defaultBranch: r.defaultBranch,
                            branches: r.branches,
                            mock: r,
                          })
                        }
                        className={cn(
                          "flex w-full items-center gap-2.5 px-3 py-2 text-left transition-colors",
                          selected ? "bg-primary/5" : "hover:bg-muted/50"
                        )}
                      >
                        <GitBranch className="size-4 shrink-0 text-muted-foreground" />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate font-mono text-sm text-foreground">
                            {r.fullName}
                          </span>
                          <span className="block truncate text-xs text-muted-foreground">
                            {r.description}
                          </span>
                        </span>
                        <Badge variant="outline" className="shrink-0 font-mono text-[10px]">
                          {r.private ? "private" : "public"}
                        </Badge>
                      </button>
                    </li>
                  );
                })}
              </ul>
            )}
          </div>
        </div>
      )}

      {detecting && (
        <p className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" />
          Reading the repository…
        </p>
      )}

      {detectError && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
          <p className="min-w-0 text-xs text-destructive/90">{detectError}</p>
        </div>
      )}

      {/* Branch selection. The default branch is preselected because it is what
          gets mapped to this environment for push-to-deploy — the wizard used
          to pick it silently and never say which. */}
      {repo && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="wizard-branch">Branch</Label>
          <Input
            id="wizard-branch"
            value={branch}
            onChange={(e) => onBranchChange(e.target.value)}
            className="font-mono"
            placeholder={repo.defaultBranch}
            spellCheck={false}
          />
          <div className="flex flex-wrap items-center gap-1.5">
            {(repo.branches ?? [repo.defaultBranch]).map((b) => (
              <Button
                key={b}
                type="button"
                variant={branch === b ? "secondary" : "outline"}
                size="sm"
                className="h-6 px-2 font-mono text-[11px]"
                onClick={() => onBranchChange(b)}
              >
                {b}
                {b === repo.defaultBranch && (
                  <span className="ml-1 text-muted-foreground">default</span>
                )}
              </Button>
            ))}
          </div>
          <p className="text-xs text-muted-foreground">
            Pushes to this branch deploy to the environment you pick later. You can
            add more branch mappings from the project&apos;s Git panel.
          </p>
        </div>
      )}

      {/* The escape hatch, explicitly secondary. */}
      {cpMode && (
        <div className="flex flex-col gap-2 border-t border-border pt-3">
          {!manualOpen ? (
            <Button
              variant="ghost"
              size="sm"
              className="w-fit text-muted-foreground"
              onClick={() => setManualOpen(true)}
            >
              <ChevronRight className="size-3.5" />
              Use a public or token repository instead
            </Button>
          ) : (
            <>
              <Label htmlFor="wizard-manual-repo" className="text-xs text-muted-foreground">
                Repository
              </Label>
              <div className="flex gap-2">
                <Input
                  id="wizard-manual-repo"
                  value={manualRepo}
                  onChange={(e) => onManualRepoChange(e.target.value)}
                  onKeyDown={(e) => e.key === "Enter" && onDetectManual()}
                  placeholder="owner/repository"
                  className="font-mono"
                  spellCheck={false}
                />
                <Button size="sm" className="h-9" onClick={onDetectManual} disabled={detecting}>
                  {detecting ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <Search className="size-4" />
                  )}
                  Read it
                </Button>
              </div>
              <Label htmlFor="wizard-repo-token" className="text-xs text-muted-foreground">
                Access token <span className="opacity-70">(private repositories only)</span>
              </Label>
              <Input
                id="wizard-repo-token"
                type="password"
                value={token}
                onChange={(e) => onTokenChange(e.target.value)}
                placeholder="github_pat_… / ghp_…"
                className="font-mono"
                autoComplete="off"
              />
              <p className="text-xs text-muted-foreground">
                A token needs read access to Contents, and webhook permission if you
                want pushes to deploy. It is stored encrypted.
              </p>
            </>
          )}
        </div>
      )}
    </div>
  );
}
