"use client";

import * as React from "react";
import { Eye, EyeOff } from "lucide-react";

import { cn } from "@/lib/utils";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

type AuthFieldProps = {
  id: string;
  label: string;
  type?: "text" | "email" | "password";
  value: string;
  onValueChange: (value: string) => void;
  error?: string | null;
  autoComplete?: string;
  placeholder?: string;
  autoFocus?: boolean;
  /** Optional content rendered to the right of the label (e.g. a "Forgot?" link). */
  labelAside?: React.ReactNode;
};

export function AuthField({
  id,
  label,
  type = "text",
  value,
  onValueChange,
  error,
  autoComplete,
  placeholder,
  autoFocus,
  labelAside,
}: AuthFieldProps) {
  const [reveal, setReveal] = React.useState(false);
  const isPassword = type === "password";
  const inputType = isPassword && reveal ? "text" : type;
  const errorId = `${id}-error`;

  return (
    <div className="grid gap-1.5">
      <div className="flex items-center justify-between gap-2">
        <Label htmlFor={id}>{label}</Label>
        {labelAside}
      </div>
      <div className="relative">
        <Input
          id={id}
          type={inputType}
          value={value}
          autoComplete={autoComplete}
          placeholder={placeholder}
          autoFocus={autoFocus}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? errorId : undefined}
          onChange={(e) => onValueChange(e.target.value)}
          className={cn(isPassword && "pr-9")}
        />
        {isPassword && (
          <button
            type="button"
            tabIndex={-1}
            onClick={() => setReveal((v) => !v)}
            aria-label={reveal ? "Hide password" : "Show password"}
            className="absolute inset-y-0 right-0 grid w-9 place-items-center rounded-r-lg text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:text-foreground"
          >
            {reveal ? (
              <EyeOff className="size-4" />
            ) : (
              <Eye className="size-4" />
            )}
          </button>
        )}
      </div>
      {error && (
        <p id={errorId} className="text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}
