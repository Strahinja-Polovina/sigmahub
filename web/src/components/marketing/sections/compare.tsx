import { Check, Minus } from "lucide-react";

import { Section, SectionHeading } from "@/components/marketing/primitives";
import { COMPARE } from "@/components/marketing/content";
import { cn } from "@/lib/utils";

/**
 * Renders a single value cell body:
 *  - "yes" → a Check icon (accent in the SigmaHub column, muted elsewhere)
 *  - "no"  → a faint Minus icon
 *  - otherwise → the qualifier text (e.g. "partial", "hobby tier")
 */
function CompareValue({ value, hero }: { value: string; hero: boolean }) {
  if (value === "yes") {
    return (
      <Check
        className={cn("mx-auto size-4", hero ? "text-primary" : "text-muted-foreground")}
        aria-label="Yes"
      />
    );
  }
  if (value === "no") {
    return (
      <Minus className="mx-auto size-4 text-muted-foreground/50" aria-label="No" />
    );
  }
  return (
    <span className={cn("text-xs", hero ? "text-primary" : "text-muted-foreground")}>
      {value}
    </span>
  );
}

export function Compare() {
  const [heroColumn, ...restColumns] = COMPARE.columns;

  return (
    <Section id="compare" variant="muted">
      <SectionHeading
        eyebrow={COMPARE.eyebrow}
        title={COMPARE.title}
        subtitle={COMPARE.subtitle}
        align="center"
      />

      <div className="mt-14 overflow-hidden rounded-xl border border-border bg-card shadow-sm">
        <div className="overflow-x-auto">
          <table className="w-full min-w-[720px] border-collapse text-sm">
            <thead>
              <tr className="border-b border-border">
                {/* Empty corner cell above the capability column. */}
                <th
                  scope="col"
                  className="sticky left-0 z-10 bg-card px-5 py-4 text-left align-bottom"
                >
                  <span className="sr-only">Capability</span>
                </th>

                {/* SigmaHub — the hero column header. */}
                <th
                  scope="col"
                  className="border-x border-border bg-accent px-4 py-4 text-center text-sm font-semibold text-primary"
                >
                  {heroColumn}
                </th>

                {/* Remaining competitor columns. */}
                {restColumns.map((column) => (
                  <th
                    scope="col"
                    key={column}
                    className="px-4 py-4 text-center text-sm font-medium text-muted-foreground"
                  >
                    {column}
                  </th>
                ))}
              </tr>
            </thead>

            <tbody>
              {COMPARE.rows.map((row, rowIndex) => {
                const [heroValue, ...restValues] = row.values;
                const isLast = rowIndex === COMPARE.rows.length - 1;

                return (
                  <tr
                    key={row.capability}
                    className={cn(
                      !isLast && "border-b border-border",
                      rowIndex % 2 === 1 && "bg-muted/30",
                    )}
                  >
                    {/* Capability label — sticky-ish left column. */}
                    <th
                      scope="row"
                      className={cn(
                        "sticky left-0 z-10 min-w-[220px] px-5 py-4 text-left align-middle text-sm font-medium text-foreground",
                        rowIndex % 2 === 1 ? "bg-muted/30" : "bg-card",
                      )}
                    >
                      {row.capability}
                    </th>

                    {/* SigmaHub value — accent carried down the whole column. */}
                    <td className="border-x border-border bg-accent px-4 py-4 text-center align-middle">
                      <CompareValue value={heroValue} hero />
                    </td>

                    {/* Competitor values. */}
                    {restValues.map((value, i) => (
                      <td
                        key={`${row.capability}-${i}`}
                        className="px-4 py-4 text-center align-middle"
                      >
                        <CompareValue value={value} hero={false} />
                      </td>
                    ))}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>
    </Section>
  );
}
