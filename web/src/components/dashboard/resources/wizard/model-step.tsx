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
  gatedWarning,
  looksLikeRepoId,
  parameterText,
  unservableReason,
  vramNeedText,
  type ModelCard,
} from "@/lib/wizard/llm";
import { llmEngineLabel } from "@/lib/wizard/managed";
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
  projectId,
  name,
  onNameChange,
  modelId,
  card,
  onModelChange,
  tokenConfigured,
  onTokenConfiguredChange,
}: {
  orgId: string;
  /** The project this endpoint is being created in, when it is known this early
   *  — the wizard was opened from a project page. It is what the search asks the
   *  weights-token question ABOUT, so a project holding its own
   *  HUGGING_FACE_HUB_TOKEN secret is not told that none is configured. Empty
   *  from /dashboard/resources, where the project is picked two steps later:
   *  there the control plane answers on its own token, which is all it can. */
  projectId: string;
  name: string;
  onNameChange: (value: string) => void;
  /** The model reference that will be sent at create — a repo id, picked or
   *  typed. */
  modelId: string;
  /** Its card, when the catalogue could confirm it. Null is a supported state:
   *  see the unresolved notice below. */
  card: ModelCard | null;
  onModelChange: (id: string, card: ModelCard | null) => void;
  /** Whether a HUGGING_FACE_HUB_TOKEN exists to pull weights with — the search
   *  publishes it. It changes what this step SAYS about a gated model and never
   *  whether the step can be left: see gatedWarning. */
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

  // The project is part of the key, not just of the request: it changes the
  // ANSWER (whether a weights token was found), so a result recorded under one
  // project must not be shown as the answer for another.
  const searchKey = `${reloads}:${projectId}:${query}`;
  const loading = result?.key !== searchKey;
  const models = result?.models ?? [];

  React.useEffect(() => {
    let cancelled = false;
    // Debounced INSIDE the effect so React owns the lifecycle: the cleanup both
    // cancels the pending request and discards a response that arrives after
    // the query moved on, which is what stops an older, slower search from
    // overwriting a newer one.
    const timer = setTimeout(() => {
      void searchModels({ orgId, query, projectId: projectId || undefined })
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
  }, [orgId, projectId, query, searchKey, onTokenConfiguredChange]);

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

  const refusal = unservableReason(card);
  const gateWarning = gatedWarning(card, tokenConfigured);
  // What the picked model IS, in one line. The runtime is DERIVED and shown
  // rather than asked: it is the one the control plane will render, and every
  // other answer the old dropdown accepted was a container that would not start.
  // A REFUSED model gets no runtime line at all — "Served by vLLM" printed above
  // a sentence explaining that vLLM cannot load these weights is the same
  // self-contradiction the token notice used to print two elements higher.
  const summary = card
    ? [
        refusal ? null : `Served by ${llmEngineLabel(card.engine)}`,
        card.parametersKnown
          ? `needs about ${vramNeedText(card)} of VRAM`
          : "no size published, so no fit check",
      ]
        .filter(Boolean)
        .join(" · ")
    : null;

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

      {/* Gated repositories ARE in the list above, with a lock on them, because
          the Hub publishes their METADATA to anyone and gates only the weights —
          demo mode ships three of them and this notice used to claim, directly
          above their rows, that they were not listed. The token that matters is
          the one the GPU host pulls with, so that is the one named here, in both
          of the places it can live: opened from a project, the search asked
          about that project's secrets; opened from the resources list, no
          project is known yet and only the control plane's own token counted. */}
      {!tokenConfigured && (
        <p className="text-xs text-muted-foreground">
          Gated repositories are listed here — Hugging Face describes them to
          anyone and gates only the download. Their weights need an account that
          has accepted the model&rsquo;s licence, and a token from it in a
          HUGGING_FACE_HUB_TOKEN secret on the project you deploy into, or on the
          control plane itself; none was found, so a gated model may fail on its
          first pull.
        </p>
      )}

      {modelId && (
        <div className="flex flex-col gap-2 rounded-lg border border-border bg-muted/40 p-3">
          <p className="font-mono text-sm text-foreground">{modelId}</p>
          {card ? (
            <p className="text-xs text-muted-foreground">{summary}</p>
          ) : (
            <p className="text-xs text-muted-foreground">
              {unresolved ?? "Served by the runtime this control plane picks for it."}
            </p>
          )}
          {/* The licence, named and linked (SIGMA-302). These weights are
              downloaded onto the customer's OWN host, and the terms bind
              whoever runs the model — the Llama Community Licence carries an
              acceptable-use policy, an attribution requirement and a 700M-MAU
              clause. None of it appeared anywhere in the product. An empty
              licence is the Hub declining to say (a gated repo read without a
              token), so it is reported as unknown and the link still goes to the
              page that states it. */}
          {card?.url && (
            <p className="text-xs text-muted-foreground">
              Licence:{" "}
              {card.license ? (
                <span className="font-mono">{card.license}</span>
              ) : (
                "not stated by the Hub"
              )}{" "}
              —{" "}
              <a
                href={card.url}
                target="_blank"
                rel="noopener noreferrer"
                className="underline underline-offset-2"
              >
                read it on the model card
              </a>
              . The weights are downloaded onto your own server under those terms.
            </p>
          )}
          <p className="text-xs text-muted-foreground">
            Pulled on first start, so the endpoint takes a few minutes to become ready.
            It listens on the private mesh only — never a public interface.
          </p>
        </div>
      )}

      {/* A refusal, in destructive red: this model cannot be served by anything
          the control plane deploys, and Continue is blocked on it. */}
      {refusal && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
          <p className="min-w-0 text-xs text-destructive/90">{refusal}</p>
        </div>
      )}

      {/* A WARNING, in amber, and Continue stays live behind it. We cannot see
          whether this operator holds access to a gated repository, and a wall
          built on that guess is a dead end for everyone who does. */}
      {gateWarning && (
        <div className="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
          <Lock className="mt-0.5 size-4 shrink-0 text-amber-600" />
          <p className="min-w-0 text-xs text-muted-foreground">{gateWarning}</p>
        </div>
      )}
    </div>
  );
}
