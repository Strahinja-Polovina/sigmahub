import Link from "next/link";
import { ArrowRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Section, GridPattern } from "@/components/marketing/primitives";
import { CLOSING } from "@/components/marketing/content";

export function ClosingCta() {
  return (
    <Section
      variant="tinted"
      className="relative overflow-hidden"
      containerClassName="relative"
    >
      {/* Decorative grid + a soft blue wash rising from the base. */}
      <GridPattern className="opacity-[0.5]" />
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 bottom-0 h-[360px] bg-[radial-gradient(60%_100%_at_50%_100%,var(--color-accent),transparent_70%)]"
      />

      <div className="relative mx-auto max-w-2xl text-center">
        <h2 className="text-balance text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
          {CLOSING.title}
        </h2>

        <p className="mt-4 text-pretty text-base leading-relaxed text-muted-foreground sm:text-lg">
          {CLOSING.subtitle}
        </p>

        <div className="mt-8 flex flex-col items-center justify-center gap-3 sm:flex-row">
          <Button
            size="lg"
            className="h-11 px-5 text-[0.95rem]"
            nativeButton={false}
            render={<Link href={CLOSING.primaryCta.href} />}
          >
            {CLOSING.primaryCta.label}
            <ArrowRight />
          </Button>
          <Button
            size="lg"
            variant="outline"
            className="h-11 px-5 text-[0.95rem]"
            nativeButton={false}
            render={<Link href={CLOSING.secondaryCta.href} />}
          >
            {CLOSING.secondaryCta.label}
          </Button>
        </div>
      </div>
    </Section>
  );
}
