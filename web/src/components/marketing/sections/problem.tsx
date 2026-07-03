import {
  Card,
  IconChip,
  Section,
  SectionHeading,
} from "@/components/marketing/primitives";
import { PROBLEM } from "@/components/marketing/content";

export function Problem() {
  return (
    <Section variant="default">
      <SectionHeading
        eyebrow={PROBLEM.eyebrow}
        title={PROBLEM.title}
        subtitle={PROBLEM.subtitle}
        align="center"
      />

      <div className="mt-14 grid grid-cols-1 gap-5 md:grid-cols-3">
        {PROBLEM.cards.map((card) => (
          <Card key={card.title} interactive className="flex flex-col">
            <IconChip>
              <card.icon className="size-5" aria-hidden />
            </IconChip>
            <h3 className="mt-4 font-semibold text-foreground">{card.title}</h3>
            <p className="mt-2 text-sm leading-relaxed text-muted-foreground">
              {card.body}
            </p>
          </Card>
        ))}
      </div>
    </Section>
  );
}
