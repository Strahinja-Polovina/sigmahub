import Link from "next/link";
import { Check } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Section, SectionHeading } from "@/components/marketing/primitives";
import { PRICING } from "@/components/marketing/content";
import { cn } from "@/lib/utils";

export function Pricing() {
  return (
    <Section id="pricing" variant="muted">
      <SectionHeading
        eyebrow={PRICING.eyebrow}
        title={PRICING.title}
        subtitle={PRICING.subtitle}
        align="center"
      />

      <div className="mx-auto mt-14 grid max-w-3xl grid-cols-1 items-stretch gap-6 sm:grid-cols-2">
        {PRICING.tiers.map((tier) => (
          <PricingCard key={tier.name} tier={tier} />
        ))}
      </div>

      <p className="mx-auto mt-8 max-w-2xl text-center text-sm text-muted-foreground">
        {PRICING.footnote}
      </p>
    </Section>
  );
}

function PricingCard({ tier }: { tier: (typeof PRICING.tiers)[number] }) {
  const { featured } = tier;

  return (
    <div
      className={cn(
        "relative flex h-full flex-col rounded-xl border bg-card p-6 shadow-sm",
        featured
          ? "border-primary/40 shadow-md ring-2 ring-primary lg:-translate-y-2"
          : "border-border",
      )}
    >
      {featured ? (
        <span className="absolute -top-3 left-1/2 inline-flex -translate-x-1/2 items-center rounded-full bg-primary px-3 py-1 text-xs font-medium text-primary-foreground shadow-sm">
          Most popular
        </span>
      ) : null}

      {/* Plan header */}
      <h3 className="text-sm font-semibold tracking-tight text-foreground">
        {tier.name}
      </h3>

      <div className="mt-4 flex items-baseline gap-1.5">
        <span className="text-4xl font-semibold tracking-tight text-foreground">
          {tier.price}
        </span>
        <span className="text-sm text-muted-foreground">{tier.unit}</span>
      </div>

      <p className="mt-3 text-sm text-muted-foreground">{tier.tagline}</p>

      {/* CTA */}
      <Button
        className="mt-6 w-full"
        variant={featured ? "default" : "outline"}
        nativeButton={false}
        render={<Link href={tier.cta.href} />}
      >
        {tier.cta.label}
      </Button>

      {/* Feature list */}
      <div className="mt-6 border-t border-border pt-6">
        <ul className="space-y-3">
          {tier.features.map((feature) => (
            <li key={feature} className="flex items-start gap-3">
              <Check className="mt-0.5 size-4 shrink-0 text-primary" aria-hidden />
              <span className="text-sm text-foreground/90">{feature}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
