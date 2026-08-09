"use client";

import * as React from "react";
import { Check, CircleAlert, Cpu, Loader2, Lock, RefreshCw, Search } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  downloadsText,
  gatedBlocked,
  looksLikeRepoId,
  parameterText,
  vramNeedText,
  type ModelCard,
} from "@/lib/wizard/llm";
import { LLM_ENGINES } from "@/lib/wizard/managed";
import { resolveModel, searchModels } from "@/server/actions/llm";
import { ResourceNameField } from "./resource-name-field";

/**
 * Pick the model, and let the model answer everything else (SIGMA-213).
 *
 * This step replaces two fields. One was a "Runtime" dropdown — a question with
 * exactly one correct answer that the operator had to look up, where vLLM
 * against a GGUF repository and Ollama against a safetensors one both produce a
 * container that will not start. It is gone: the card carries the runtime the
 * control plane would render, and this screen states it. The other was a
 * free-text model field, which turned a typo, a gated repository or an oversized
 * model into an identical outcome — a resource that exists, bills at GPU rates
 * and dies while pulling.
 *
 * Typing a repo id still works, because a picker that cannot express "the model
 * I already know the name of" is a worse field than the one it replaced. What
 * changed is that a typed id is RESOLVED, so it is sized and fit-checked exactly
 * like a picked one.
 */

/** Long enough that an ordinary typing burst is one request, short enough that
 *  the list feels attached to the keyboard. */
const SEARCH_DEBOUNCE_MS = 300;

const ENGINE_LABELS = new Map(LLM_ENGINES.map((e) => [e.id as string, e.label as string]));

/** The results, tagged with the search they answer.
 *
 *  Tagged rather than accompanied by a `loading` flag so that "is this list the
 *  answer to what is in the box" is a comparison and not a second piece of state
 *  to keep in step — which is how a picker ends up spinning forever after a race
 *  it did not notice. */
type SearchResult = {
  key: string;
  models: ModelCard[];
  /** A lookup that FAILED, as opposed to one that found nothing. Only this one
   *  gets a Retry — "no matches for qwn3" is not retryable, it is a typo. */
  error: string | null;
};

export function ModelStep({
  orgId,
  name,
  onNameChange,
  modelId,
  card,
  onModelChange,
  tokenConfigured,
  onTokenConfiguredChange,
}: {
  orgId: string;
  name: string;
  onNameChange: (value: string) => void;
  /** The model reference that will be sent at create — a repo id, picked or
   *  typed. */
  modelId: string;
  /** Its card, when the catalogue could confirm it. Null is a supported state:
   *  see the unresolved notice below. */
  card: ModelCard | null;
  onModelChange: (id: string, card: ModelCard | null) => void;
  tokenConfigured: boolean;
  onTokenConfiguredChange: (value: boolean) => void;
}) {
  const [query, setQuery] = React.useState("");
  const [reloads, setReloads] = React.useState(0);
  const [result, setResult] = React.useState<SearchResult | null>(null);
  const [resolving, setResolving] = React.useState(false);
  /** What to say about a typed id the catalogue did not confirm — the lookup's
   *  own reason when it failed, the plain fact when the Hub simply does not know
   *  it. Neither is a block: the control plane uses the reference exactly as
   *  typed and skips the fit check. */
  const [unresolved, setUnresolved] = React.useState<string | null>(null);

  const searchKey = `${reloads}:${query}`;
  const loading = result?.key !== searchKey;
  const models = result?.models ?? [];

  React.useEffect(() => {
    let cancelled = false;
    // Debounced INSIDE the effect so React owns the lifecycle: the cleanup both
    // cancels the pending request and discards a response that arrives after
    // the query moved on, which is what stops an older, slower search from
    // overwriting a newer one.
    const timer = setTimeout(() => {
      void searchModels({ orgId, query })
        .then((res) => {
          if (cancelled) return;
          setResult({ key: searchKey, models: res.models, error: res.error ?? null });
          onTokenConfiguredChange(res.tokenConfigured);
        })
        .catch((err: unknown) => {
          if (cancelled) return;
          setResult({
            key: searchKey,
            models: [],
            error:
              err instanceof Error
                ? err.message
                : "Couldn't reach the model catalogue. Try again, or type the repo id.",
          });
        });
    }, SEARCH_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [orgId, query, searchKey, onTokenConfiguredChange]);

  const typed = query.trim();
  /** Offer the typed text as an id only when it could BE one and the search did
   *  not already return it — otherwise the offer duplicates a row that is one
   *  click away. */
  const offerTyped =
    looksLikeRepoId(typed) && typed !== modelId && !models.some((m) => m.id === typed);

  async function applyTypedId() {
    const id = typed;
    setResolving(true);
    setUnresolved(null);
    try {
      const res = await resolveModel({ orgId, id });
      // A null card is an answer, not a failure: the id goes through as typed
      // and the fit check simply does not apply to it.
      onModelChange(id, res.card);
      if (!res.card) {
        setUnresolved(
          res.error ??
            "Not confirmed on the Hub — it will be used exactly as typed, and no VRAM check applies to it."
        );
      }
    } finally {
      setResolving(false);
    }
  }

  const gateReason = gatedBlocked(card, tokenConfigured);
  const engineLabel = card ? (ENGINE_LABELS.get(card.engine) ?? card.engine) : null;

  return (
    <div className="flex flex-col gap-4">
      <ResourceNameField
        id="wizard-llm-name"
        value={name}
        onChange={onNameChange}
        hint="Other resources reach this endpoint by this name on the private network."
      />

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="wizard-model-search">Model</Label>
        <div className="flex gap-2">
          <div className="relative flex-1">
            <Search className="absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input
              id="wizard-model-search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search Hugging Face, or paste owner/model"
              className="pl-8 font-mono"
              spellCheck={false}
              autoComplete="off"
            />
          </div>
          {loading && (
            <span className="grid size-9 shrink-0 place-items-center text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
            </span>
          )}
        </div>
      </div>

      {result?.error && (
        <div className="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-amber-600" />
          <div className="flex min-w-0 flex-1 flex-col items-start gap-2">
            <p className="text-xs text-muted-foreground">{result.error}</p>
            <Button
              variant="outline"
              size="sm"
              className="h-7"
              onClick={() => setReloads((n) => n + 1)}
            >
              <RefreshCw className="size-3.5" />
              Try again
            </Button>
          </div>
        </div>
      )}

      <div className="max-h-64 overflow-y-auto rounded-lg border border-border">
        {models.length === 0 ? (
          <p className="p-4 text-center text-xs text-muted-foreground">
            {loading
              ? "Searching the Hub…"
              : typed
                ? `No model matches “${typed}”.`
                : "No models to list. Paste a repository id to use one directly."}
          </p>
        ) : (
          <ul className="divide-y divide-border">
            {models.map((model) => {
              const selected = modelId === model.id;
              return (
                <li key={model.id}>
                  <button
                    type="button"
                    aria-pressed={selected}
                    onClick={() => {
                      setUnresolved(null);
                      onModelChange(model.id, model);
                    }}
                    className={cn(
                      "flex w-full items-start gap-2.5 px-3 py-2.5 text-left transition-colors",
                      selected ? "bg-primary/5" : "hover:bg-muted/50"
                    )}
                  >
                    <span className="min-w-0 flex-1">
                      <span className="flex items-center gap-2">
                        <span className="truncate text-sm font-medium text-foreground">
                          {model.name}
                        </span>
                        {model.gated && (
                          <Badge variant="outline" className="shrink-0 gap-1 text-[10px]">
                            <Lock className="size-3" />
                            Gated
                          </Badge>
                        )}
                      </span>
                      <span className="block truncate font-mono text-xs text-muted-foreground">
                        {model.id}
                      </span>
                      {/* vramText is the control plane's own rendering of the
                          estimate. Printed, never recomputed: the create-time
                          refusal quotes the same string. */}
                      <span className="block truncate text-xs text-muted-foreground">
                        {parameterText(model)}
                        {model.vramText ? ` · ${model.vramText} VRAM` : ""} ·{" "}
                        {downloadsText(model.downloads)}
                      </span>
                    </span>
                    {selected && <Check className="mt-0.5 size-4 shrink-0 text-primary" />}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>

      {offerTyped && (
        <Button
          variant="outline"
          size="sm"
          className="w-fit"
          disabled={resolving}
          onClick={() => void applyTypedId()}
        >
          {resolving ? <Loader2 className="size-4 animate-spin" /> : <Cpu className="size-4" />}
          Use <span className="font-mono">{typed}</span>
        </Button>
      )}

      {!tokenConfigured && (
        <p className="text-xs text-muted-foreground">
          This control plane holds no Hugging Face token, so gated repositories are
          not listed here and their weights cannot be downloaded.
        </p>
      )}

      {modelId && (
        <div className="flex flex-col gap-2 rounded-lg border border-border bg-muted/40 p-3">
          <p className="font-mono text-sm text-foreground">{modelId}</p>
          {card ? (
            <p className="text-xs text-muted-foreground">
              {/* The runtime is DERIVED, and shown rather than asked: it is the
                  one the control plane will render for this model, and every
                  other answer was a container that would not start. */}
              Served by {engineLabel}
              {card.parametersKnown
                ? ` · needs about ${vramNeedText(card)} of VRAM`
                : " · no size published, so no fit check"}
            </p>
          ) : (
            <p className="text-xs text-muted-foreground">
              {unresolved ?? "Served by the runtime this control plane picks for it."}
            </p>
          )}
          <p className="text-xs text-muted-foreground">
            Pulled on first start, so the endpoint takes a few minutes to become ready.
            It listens on the private mesh only — never a public interface.
          </p>
        </div>
      )}

      {gateReason && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
          <p className="min-w-0 text-xs text-destructive/90">{gateReason}</p>
        </div>
      )}
    </div>
  );
}
