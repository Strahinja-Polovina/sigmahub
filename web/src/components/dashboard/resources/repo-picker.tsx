"use client";

import * as React from "react";
import { Loader2, Lock, Globe, Search, FolderGit2, RefreshCw, CircleAlert } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { listGitRepos } from "@/server/actions/git";
import type { CpSelectableRepo } from "@/server/cp";

/**
 * Pick a repository from the org's GitHub integration.
 *
 * The point of the org-level integration is that a repo is SELECTED, never
 * connected by hand: the App is installed once and everything it grants shows
 * up here. When nothing is connected the caller falls back to the manual
 * owner/repo + token form, so this never becomes a dead end.
 */
export function RepoPicker({
  orgId,
  value,
  onSelect,
  onUnavailable,
}: {
  orgId: string;
  /** Currently selected repo full name, if any. */
  value?: string | null;
  onSelect: (repo: CpSelectableRepo) => void;
  /** Called once when the org has no usable integration, so the caller can
   *  show the manual path (and the Connect-GitHub offer) instead of an empty
   *  list. NOT called when the control plane simply failed to answer — see the
   *  "failed" status below. */
  onUnavailable?: () => void;
}) {
  // One state object: the load is a single external read, so the results land
  // together and there is no window where the list is set but "loading" isn't.
  //
  // "failed" is a third status, not an empty "ready" (SIGMA-237). An empty list
  // means "the control plane says you have nothing here", which the wizard
  // answers by offering to install the GitHub App. A failed read means we do not
  // know, and the only honest offer is to ask again — sending someone to
  // github.com to re-run an org-wide App installation because the CP was
  // restarting can permanently narrow that org's repository access.
  const [state, setState] = React.useState<{
    status: "loading" | "ready" | "failed";
    repos: CpSelectableRepo[];
    truncated: boolean;
    unavailable: string[];
    error: string | null;
  }>({ status: "loading", repos: [], truncated: false, unavailable: [], error: null });

  const [query, setQuery] = React.useState("");
  // `reloads` is bumped by the refresh button; the fetch itself lives in the
  // effect so React owns its lifecycle and a stale response can be discarded.
  const [reloads, setReloads] = React.useState(0);

  // Latest callback without making it a dependency: re-running the fetch on
  // every parent re-render would hammer GitHub's API for no reason.
  const unavailableRef = React.useRef(onUnavailable);
  React.useEffect(() => {
    unavailableRef.current = onUnavailable;
  }, [onUnavailable]);

  React.useEffect(() => {
    let cancelled = false;

    async function load() {
      const fail = (message: string) => {
        setState({
          status: "failed",
          repos: [],
          truncated: false,
          unavailable: [],
          error: message,
        });
      };

      try {
        const res = await listGitRepos(orgId);
        if (cancelled) return;
        // The action reports a control-plane failure in `error`. It is NOT an
        // answer about the org's integration, so it must not reach
        // onUnavailable — that switch raises the install offer.
        if (res.error) {
          fail(res.error);
          return;
        }
        setState({
          status: "ready",
          repos: res.repos,
          truncated: Boolean(res.truncated),
          unavailable: res.unavailable ?? [],
          error: null,
        });
        if (!res.connected || res.repos.length === 0) unavailableRef.current?.();
      } catch (err) {
        if (cancelled) return;
        // A throw here is transport too — in production Next.js has already
        // redacted the message to a digest, which is exactly why we cannot
        // pretend it means "GitHub isn't connected".
        fail(
          `Couldn't reach the control plane to list your repositories${
            err instanceof Error && err.message ? `: ${err.message}` : ""
          }.`
        );
      }
    }

    void load();
    return () => {
      cancelled = true;
    };
  }, [orgId, reloads]);

  const { status, repos, truncated, unavailable, error } = state;
  const loading = status === "loading";

  const filtered = React.useMemo(() => {
    if (!repos) return [];
    const q = query.trim().toLowerCase();
    if (!q) return repos;
    return repos.filter((r) => r.fullName.toLowerCase().includes(q));
  }, [repos, query]);

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-border p-4 text-sm text-muted-foreground">
        <Loader2 className="size-4 animate-spin" />
        Loading your repositories…
      </div>
    );
  }

  // The third state (SIGMA-237): we could not ask. Say so, offer a retry, and
  // never the install — the org's integration may well be perfectly fine.
  if (status === "failed") {
    return (
      <div className="flex flex-col gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-4">
        <div className="flex items-start gap-2">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium text-foreground">
              Couldn’t reach the control plane to list your repositories
            </p>
            <p className="text-xs text-muted-foreground">
              {error} This says nothing about your GitHub connection — if the app is
              installed, your repositories are still there. Try again in a moment.
            </p>
          </div>
        </div>
        <Button
          variant="outline"
          size="sm"
          className="w-fit"
          onClick={() => {
            // Back to the spinner so the click has visible feedback and the
            // stale error goes away; the effect below re-reads on `reloads`.
            setState({
              status: "loading",
              repos: [],
              truncated: false,
              unavailable: [],
              error: null,
            });
            setReloads((n) => n + 1);
          }}
        >
          <RefreshCw className="size-4" />
          Retry
        </Button>
      </div>
    );
  }

  if (repos.length === 0) {
    return null; // The caller renders the manual fallback.
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex gap-2">
        <div className="relative flex-1">
          <Search className="absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Filter repositories"
            className="pl-8 font-mono"
            aria-label="Filter repositories"
          />
        </div>
        <Button
          variant="outline"
          size="sm"
          className="h-9 shrink-0"
          onClick={() => setReloads((n) => n + 1)}
          aria-label="Refresh repositories"
        >
          <RefreshCw className="size-4" />
        </Button>
      </div>

      <div className="max-h-56 overflow-y-auto rounded-lg border border-border">
        {filtered.length === 0 ? (
          <p className="p-4 text-center text-sm text-muted-foreground">
            No repository matches “{query}”.
          </p>
        ) : (
          <ul className="divide-y divide-border">
            {filtered.map((repo) => {
              const selected = value === repo.fullName;
              return (
                <li key={repo.fullName}>
                  <button
                    type="button"
                    onClick={() => onSelect(repo)}
                    aria-pressed={selected}
                    className={cn(
                      "flex w-full items-center gap-2.5 px-3 py-2 text-left transition-colors",
                      selected ? "bg-primary/5" : "hover:bg-muted/50"
                    )}
                  >
                    <FolderGit2 className="size-4 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 flex-1 truncate font-mono text-sm text-foreground">
                      {repo.fullName}
                    </span>
                    {repo.private ? (
                      <Lock className="size-3.5 shrink-0 text-muted-foreground" aria-label="Private" />
                    ) : (
                      <Globe className="size-3.5 shrink-0 text-muted-foreground" aria-label="Public" />
                    )}
                    {selected && (
                      <Badge variant="outline" className="shrink-0 text-[10px]">
                        Selected
                      </Badge>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>

      {truncated && (
        <p className="text-xs text-muted-foreground">
          Showing the first {repos.length} repositories. Filter by name to find one
          that isn’t listed.
        </p>
      )}
      {unavailable.length > 0 && (
        <p className="text-xs text-amber-700 dark:text-amber-500">
          Couldn’t read repositories from {unavailable.join(", ")} — that account’s
          list may be incomplete.
        </p>
      )}
    </div>
  );
}
