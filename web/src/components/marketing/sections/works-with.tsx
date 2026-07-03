/**
 * "Works with your stack" trust strip.
 *
 * A compact logo/trust band — NOT a full <Section>. Renders a modest muted
 * label plus the two WORKS_WITH groups as wrapped rows of monochrome wordmark
 * pills. Understated by design: this is a trust strip, not a hero.
 */

import { Container } from "@/components/marketing/primitives";
import { WORKS_WITH } from "@/components/marketing/content";

export function WorksWith() {
  return (
    <section className="border-b border-border bg-muted/40">
      <Container>
        <div className="py-10 sm:py-12">
          <p className="text-center text-sm text-muted-foreground">
            {WORKS_WITH.heading}
          </p>

          <div className="mt-8 flex flex-col items-center gap-8 sm:mt-9 sm:flex-row sm:justify-center sm:gap-12 lg:gap-16">
            {WORKS_WITH.groups.map((group) => (
              <div
                key={group.label}
                className="flex flex-col items-center gap-3 sm:items-start"
              >
                <span className="font-mono text-[0.65rem] font-medium uppercase tracking-[0.16em] text-muted-foreground/70">
                  {group.label}
                </span>
                <ul className="flex flex-wrap justify-center gap-2 sm:justify-start">
                  {group.items.map((item) => (
                    <li
                      key={item}
                      className="rounded-full border border-border bg-background/60 px-3 py-1 text-sm font-medium text-foreground/70"
                    >
                      {item}
                    </li>
                  ))}
                </ul>
              </div>
            ))}
          </div>
        </div>
      </Container>
    </section>
  );
}
