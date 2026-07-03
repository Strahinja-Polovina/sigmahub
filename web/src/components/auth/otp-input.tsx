"use client";

import * as React from "react";

import { cn } from "@/lib/utils";

// A 6-digit segmented input for the mock TOTP step. Value is owned by the
// parent; this component only renders the boxes and handles keyboard/paste UX.

type OtpInputProps = {
  value: string;
  onChange: (value: string) => void;
  onComplete?: (value: string) => void;
  invalid?: boolean;
  disabled?: boolean;
  autoFocus?: boolean;
};

const LENGTH = 6;

export function OtpInput({
  value,
  onChange,
  onComplete,
  invalid,
  disabled,
  autoFocus,
}: OtpInputProps) {
  const refs = React.useRef<Array<HTMLInputElement | null>>([]);

  const setDigit = (index: number, digit: string) => {
    const next = value.split("");
    next[index] = digit;
    const joined = next.join("").slice(0, LENGTH);
    onChange(joined);
    return joined;
  };

  const focusAt = (index: number) => {
    const el = refs.current[Math.max(0, Math.min(LENGTH - 1, index))];
    el?.focus();
    el?.select();
  };

  const handleChange = (index: number, raw: string) => {
    const digit = raw.replace(/\D/g, "").slice(-1);
    if (!digit) return;
    const joined = setDigit(index, digit);
    if (index < LENGTH - 1) focusAt(index + 1);
    if (joined.length === LENGTH && !joined.includes("")) {
      onComplete?.(joined);
    }
  };

  const handleKeyDown = (
    index: number,
    e: React.KeyboardEvent<HTMLInputElement>,
  ) => {
    if (e.key === "Backspace") {
      e.preventDefault();
      if (value[index]) {
        setDigit(index, "");
      } else if (index > 0) {
        setDigit(index - 1, "");
        focusAt(index - 1);
      }
    } else if (e.key === "ArrowLeft") {
      e.preventDefault();
      focusAt(index - 1);
    } else if (e.key === "ArrowRight") {
      e.preventDefault();
      focusAt(index + 1);
    }
  };

  const handlePaste = (
    index: number,
    e: React.ClipboardEvent<HTMLInputElement>,
  ) => {
    e.preventDefault();
    const pasted = e.clipboardData.getData("text").replace(/\D/g, "");
    if (!pasted) return;
    const chars = value.split("");
    for (let i = 0; i < pasted.length && index + i < LENGTH; i++) {
      chars[index + i] = pasted[i];
    }
    const joined = chars.join("").slice(0, LENGTH);
    onChange(joined);
    const nextIndex = Math.min(index + pasted.length, LENGTH - 1);
    focusAt(nextIndex);
    if (joined.length === LENGTH && !joined.includes("")) {
      onComplete?.(joined);
    }
  };

  return (
    <div className="flex items-center gap-2" role="group" aria-label="One-time code">
      {Array.from({ length: LENGTH }).map((_, i) => (
        <input
          key={i}
          ref={(el) => {
            refs.current[i] = el;
          }}
          type="text"
          inputMode="numeric"
          autoComplete={i === 0 ? "one-time-code" : "off"}
          maxLength={1}
          disabled={disabled}
          autoFocus={autoFocus && i === 0}
          value={value[i] ?? ""}
          aria-invalid={invalid || undefined}
          aria-label={`Digit ${i + 1}`}
          onChange={(e) => handleChange(i, e.target.value)}
          onKeyDown={(e) => handleKeyDown(i, e)}
          onPaste={(e) => handlePaste(i, e)}
          onFocus={(e) => e.target.select()}
          className={cn(
            "h-11 w-full min-w-0 rounded-lg border border-input bg-transparent text-center font-mono text-lg tabular-nums outline-none transition-colors",
            "focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50",
            "disabled:pointer-events-none disabled:opacity-50",
            invalid &&
              "border-destructive ring-3 ring-destructive/20 aria-invalid:border-destructive",
          )}
        />
      ))}
    </div>
  );
}
