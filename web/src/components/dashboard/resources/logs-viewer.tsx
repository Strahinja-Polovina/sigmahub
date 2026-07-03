"use client";

import * as React from "react";
import { cn } from "@/lib/utils";
import type { LogLine } from "@/lib/mock";

const LEVEL_META: Record<LogLine["level"], { label: string; text: string }> = {
  info: { label: "INFO", text: "text-muted-foreground" },
  warn: { label: "WARN", text: "text-amber-600" },
  error: { label: "ERROR", text: "text-red-600" },
};

export function LogsViewer({ logs }: { logs: LogLine[] }) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-muted/30">
      <div className="max-h-[420px] overflow-y-auto p-3 font-mono text-xs leading-relaxed">
        {logs.map((line, i) => {
          const meta = LEVEL_META[line.level];
          return (
            <div
              key={i}
              className="flex items-start gap-3 whitespace-pre-wrap py-0.5"
            >
              <span className="shrink-0 tabular-nums text-muted-foreground/70">
                {line.t}
              </span>
              <span
                className={cn(
                  "w-11 shrink-0 font-semibold uppercase",
                  meta.text
                )}
              >
                {meta.label}
              </span>
              <span
                className={cn(
                  "min-w-0 flex-1",
                  line.level === "error"
                    ? "text-red-600"
                    : line.level === "warn"
                      ? "text-amber-700"
                      : "text-foreground"
                )}
              >
                {line.msg}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
