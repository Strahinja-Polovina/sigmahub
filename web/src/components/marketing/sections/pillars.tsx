import { Card, IconChip, Section, SectionHeading } from "@/components/marketing/primitives";
import { PILLARS, PILLARS_SECTION } from "@/components/marketing/content";

export function Pillars() {
  return (
    <Section id="product" variant="muted">
      <SectionHeading
        eyebrow={PILLARS_SECTION.eyebrow}
        title={PILLARS_SECTION.title}
        subtitle={PILLARS_SECTION.subtitle}
        align="center"
      />

      <div className="mt-14 grid grid-cols-1 gap-5 sm:grid-cols-2 lg:grid-cols-4">
        {PILLARS.map((pillar) => (
          <Card key={pillar.title} interactive className="flex flex-col">
            <IconChip>
              <pillar.icon className="size-5" />
            </IconChip>
            <h3 className="mt-4 text-[15px] font-semibold text-foreground">
              {pillar.title}
            </h3>
            <p className="mt-2 text-sm leading-relaxed text-muted-foreground">
              {pillar.description}
            </p>
          </Card>
        ))}
      </div>

      <p className="mx-auto mt-8 max-w-2xl text-balance text-center text-sm text-muted-foreground">
        <span className="text-foreground">Disaster recovery</span>,{" "}
        <span className="text-foreground">GPU/LLM serving</span> and{" "}
        <span className="text-foreground">Kubernetes</span> included — never
        add-ons.
      </p>
    </Section>
  );
}
