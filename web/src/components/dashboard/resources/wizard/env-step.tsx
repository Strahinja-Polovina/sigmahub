"use client";

import * as React from "react";
import { Plus, Trash2, ClipboardPaste, Eye, EyeOff, CircleAlert } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import {
  blankEnvDraft,
  envKeyValid,
  isSecretKey,
  mergeEnvVars,
  parseDotenv,
  type EnvDraft,
} from "@/lib/wizard/env";

/**
 * Environment variables, seeded and bulk-pasteable (SIGMA-211).
 *
 * Detection has always returned the repository's own variable NAMES; the old
 * step used them and stopped there. Two things were still missing: marking the
 * ones that are credentials — a password typed into a plain field ends up in a
 * screenshot — and pasting a whole .env, which is how everybody actually moves
 * forty variables.
 */
export function EnvStep({
  vars,
  onChange,
  seededFromRepo,
}: {
  vars: EnvDraft[];
  onChange: (vars: EnvDraft[]) => void;
  seededFromRepo: boolean;
}) {
  const [pasteOpen, setPasteOpen] = React.useState(false);
  const [pasteText, setPasteText] = React.useState("");
  const [pasteErrors, setPasteErrors] = React.useState<string[]>([]);
  const [revealed, setRevealed] = React.useState<Set<string>>(new Set());

  function update(id: string, patch: Partial<EnvDraft>) {
    onChange(vars.map((v) => (v.id === id ? { ...v, ...patch } : v)));
  }

  function applyPaste() {
    const parsed = parseDotenv(pasteText);
    // Errors are REPORTED, not swallowed: a paste that silently loses three of
    // forty variables is a container that dies on start for a reason nothing
    // here mentioned.
    setPasteErrors(parsed.errors.map((e) => `Line ${e.line}: ${e.reason} — “${e.text}”`));
    if (parsed.vars.length > 0) onChange(mergeEnvVars(vars, parsed.vars));
    if (parsed.errors.length === 0) {
      setPasteOpen(false);
      setPasteText("");
    }
  }

  const anyInvalid = vars.some((v) => !envKeyValid(v.key));

  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-muted-foreground">
        Injected into the container at runtime.
        {seededFromRepo
          ? " Keys were pre-filled from the repository (.env.example, Dockerfile ENV/ARG, compose environment) — fill in the values."
          : ""}
      </p>

      <div className="flex flex-col gap-2">
        {vars.map((v) => {
          const show = !v.secret || revealed.has(v.id);
          return (
            <div key={v.id} className="grid grid-cols-[1fr_1fr_auto_auto] items-center gap-2">
              <Input
                value={v.key}
                onChange={(e) => {
                  const key = e.target.value.toUpperCase();
                  // Re-deriving `secret` while the user types keeps the mark
                  // honest as a key becomes DB_PASSWORD — but only until they
                  // set it themselves, which the toggle below records.
                  update(v.id, { key, secret: v.touchedSecret ? v.secret : isSecretKey(key) });
                }}
                placeholder="KEY"
                className="font-mono"
                aria-label="Variable name"
                aria-invalid={!envKeyValid(v.key) || undefined}
              />
              <Input
                value={v.value}
                type={show ? "text" : "password"}
                onChange={(e) => update(v.id, { value: e.target.value })}
                placeholder="value"
                className="font-mono"
                aria-label="Value"
                autoComplete="off"
              />
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={v.secret ? `Stop treating ${v.key || "this variable"} as a secret` : `Treat ${v.key || "this variable"} as a secret`}
                aria-pressed={v.secret}
                onClick={() => {
                  update(v.id, { secret: !v.secret, touchedSecret: true });
                  if (v.secret) {
                    setRevealed((prev) => {
                      const next = new Set(prev);
                      next.delete(v.id);
                      return next;
                    });
                  }
                }}
                className={cn(v.secret && "text-primary")}
              >
                {v.secret ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
              </Button>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="Remove variable"
                onClick={() => onChange(vars.filter((x) => x.id !== v.id))}
              >
                <Trash2 className="size-4 text-muted-foreground" />
              </Button>
            </div>
          );
        })}
      </div>

      {anyInvalid && (
        <p className="flex items-center gap-1.5 text-xs text-destructive">
          <CircleAlert className="size-3.5 shrink-0" />
          Keys must start with a letter or underscore and contain only letters, digits
          and underscores (e.g. API_KEY).
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button variant="outline" size="sm" onClick={() => onChange([...vars, blankEnvDraft()])}>
          <Plus className="size-3.5" />
          Add variable
        </Button>
        <Button variant="outline" size="sm" onClick={() => setPasteOpen((o) => !o)}>
          <ClipboardPaste className="size-3.5" />
          Paste .env
        </Button>
      </div>

      {pasteOpen && (
        <div className="flex flex-col gap-2 rounded-lg border border-border p-3">
          <Label htmlFor="wizard-env-paste" className="text-xs text-muted-foreground">
            Paste a .env file
          </Label>
          <Textarea
            id="wizard-env-paste"
            value={pasteText}
            onChange={(e) => setPasteText(e.target.value)}
            placeholder={"DATABASE_URL=postgres://…\n# comments and blank lines are ignored\nAPI_KEY=\"quoted values work\""}
            className="min-h-28 font-mono text-xs"
            spellCheck={false}
          />
          {pasteErrors.length > 0 && (
            <ul className="flex flex-col gap-0.5 text-xs text-destructive">
              {pasteErrors.map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          )}
          <div className="flex items-center gap-2">
            <Button size="sm" onClick={applyPaste} disabled={!pasteText.trim()}>
              Add variables
            </Button>
            <span className="text-xs text-muted-foreground">
              A key that already exists has its value filled in rather than duplicated.
            </span>
          </div>
        </div>
      )}

      <p className="text-xs text-muted-foreground">
        Values are encrypted at rest. Anything marked with the eye is masked here and
        stored as a secret — the resource&apos;s Secrets panel can rotate it afterwards.
      </p>
    </div>
  );
}
