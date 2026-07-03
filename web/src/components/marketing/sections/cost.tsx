import * as React from "react";
import { ArrowRight } from "lucide-react";

import { Section, SectionHeading } from "@/components/marketing/primitives";
import { COST } from "@/components/marketing/content";
import { cn } from "@/lib/utils";

/**
 * Emphasize the load-bearing figures inside the kicker without restating the
 * copy: split the canonical string on the phrases we want to highlight and wrap
 * only those spans. If a phrase is ever edited out of content.ts, it simply
 * renders as plain text.
 */
function renderKicker(text: string): React.ReactNode {
  const emphatic: Record<string, string> = {
    "€50/month": "font-semibold text-primary",
    included: "font-semibold text-foreground",
  };
  const parts = text.split(/(€50\/month|included)/g);
  return parts.map((part, i) =>
    emphatic[part] ? (
      <span key={i} className={emphatic[part]}>
        {part}
      </span>
    ) : (
      <React.Fragment key={i}>{part}</React.Fragment>
    ),
  );
}

export function Cost() {
  return (
    <Section variant="default">
      <SectionHeading
        eyebrow={COST.eyebrow}
        title={COST.title}
        subtitle={COST.subtitle}
        align="center"
      />

      <div className="mx-auto mt-14 max-w-4xl">
        <div className="overflow-hidden rounded-xl border border-border bg-card shadow-sm">
          {/* Column header */}
          <div className="hidden grid-cols-[1.4fr_1fr_1fr] items-center gap-4 border-b border-border bg-muted/40 px-5 py-3 sm:grid sm:px-6">
            <span className="text-xs font-medium uppercase tracking-[0.1em] text-muted-foreground">
              Cost line
            </span>
            <span className="text-xs font-medium uppercase tracking-[0.1em] text-muted-foreground">
              Managed cloud
            </span>
            <span className="text-xs font-medium uppercase tracking-[0.1em] text-primary">
              On your own servers
            </span>
          </div>

          {/* Rows */}
          {COST.rows.map((row, i) => (
            <div
              key={row.line}
              className={cn(
                "grid grid-cols-1 gap-x-4 gap-y-3 px-5 py-5 sm:grid-cols-[1.4fr_1fr_1fr] sm:items-center sm:gap-y-0 sm:px-6",
                i !== COST.rows.length - 1 && "border-b border-border",
              )}
            >
              {/* Cost line */}
              <div className="min-w-0">
                <p className="font-medium text-foreground">{row.line}</p>
              </div>

              {/* Managed cloud — the expensive side */}
              <div className="flex items-baseline gap-2 sm:block">
                <span className="text-lg font-semibold tracking-tight text-muted-foreground line-through decoration-muted-foreground/40 decoration-1">
                  {row.managed}
                </span>
                <span className="text-xs text-muted-foreground sm:mt-0.5 sm:block">
                  {row.managedNote}
                </span>
              </div>

              {/* vs arrow + owned — emphasized */}
              <div className="flex items-baseline gap-2 sm:block">
                <ArrowRight
                  className="hidden size-4 shrink-0 self-center text-muted-foreground/60 sm:mb-1 sm:inline-block"
                  aria-hidden
                />
                <span className="text-lg font-semibold tracking-tight text-primary sm:ml-0">
                  {row.owned}
                </span>
                <span className="text-xs text-muted-foreground sm:mt-0.5 sm:block">
                  {row.ownedNote}
                </span>
              </div>
            </div>
          ))}
        </div>

        {/* Highlighted callout band */}
        <div className="mt-8 rounded-lg border border-primary/20 bg-accent/50 p-5">
          <p className="text-sm leading-relaxed text-foreground sm:text-base">
            {renderKicker(COST.kicker)}
          </p>
        </div>
      </div>
    </Section>
  );
}
