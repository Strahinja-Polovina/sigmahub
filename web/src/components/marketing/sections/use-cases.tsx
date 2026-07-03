import { ArrowRight } from "lucide-react";

import {
  Card,
  IconChip,
  Section,
  SectionHeading,
} from "@/components/marketing/primitives";
import { USE_CASES } from "@/components/marketing/content";

export function UseCases() {
  return (
    <Section variant="muted">
      <SectionHeading
        eyebrow={USE_CASES.eyebrow}
        title={USE_CASES.title}
        subtitle={USE_CASES.subtitle}
        align="center"
      />

      <div className="mt-14 grid grid-cols-1 gap-5 md:grid-cols-2">
        {USE_CASES.cases.map((useCase) => (
          <Card key={useCase.audience} interactive className="flex flex-col">
            <div className="flex items-center gap-3">
              <IconChip>
                <useCase.icon className="size-5" aria-hidden />
              </IconChip>
              <h3 className="text-base font-semibold text-foreground">
                {useCase.audience}
              </h3>
            </div>

            <dl className="mt-5 flex flex-1 flex-col gap-4">
              <div>
                <dt className="font-mono text-[0.65rem] font-medium uppercase tracking-[0.14em] text-muted-foreground">
                  Today
                </dt>
                <dd className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
                  {useCase.pain}
                </dd>
              </div>

              <div className="border-t border-border pt-4">
                <dt className="font-mono text-[0.65rem] font-medium uppercase tracking-[0.14em] text-primary">
                  With SigmaHub
                </dt>
                <dd className="mt-1.5 flex gap-2 text-sm leading-relaxed text-foreground">
                  <ArrowRight
                    className="mt-0.5 size-4 shrink-0 text-primary"
                    aria-hidden
                  />
                  <span>{useCase.gain}</span>
                </dd>
              </div>
            </dl>
          </Card>
        ))}
      </div>
    </Section>
  );
}
