"use client";

import * as React from "react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { resourceNameError } from "@/lib/wizard/managed";

/**
 * The resource name, with its own validator attached.
 *
 * Extracted when the LLM path grew its own step (SIGMA-213): every managed kind
 * asks for a name on its configuration screen, and a second copy of the field is
 * a second place for the validator to be forgotten — which is how the wizard
 * once rendered a name error in red beside an enabled Continue button.
 *
 * The hint differs per kind because what the name IS differs: a container's DNS
 * label, a bucket prefix, the address other services dial.
 */
export function ResourceNameField({
  id,
  value,
  onChange,
  hint,
}: {
  id: string;
  value: string;
  onChange: (value: string) => void;
  hint: string;
}) {
  const problem = resourceNameError(value);
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>Name</Label>
      <Input
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="font-mono"
        spellCheck={false}
        aria-invalid={problem ? true : undefined}
      />
      <p className={cn("text-xs", problem ? "text-destructive" : "text-muted-foreground")}>
        {problem ?? hint}
      </p>
    </div>
  );
}
