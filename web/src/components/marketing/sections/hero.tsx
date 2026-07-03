import Link from "next/link";
import { ArrowRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Container, GridPattern, Pill } from "@/components/marketing/primitives";
import { ProductMock } from "@/components/marketing/product-mock";
import { HERO } from "@/components/marketing/content";

export function Hero() {
  return (
    <section className="relative overflow-hidden border-b border-border">
      {/* Background: faint grid, a soft accent wash, and a slowly drifting blue glow. */}
      <GridPattern className="opacity-[0.5]" />
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 top-0 h-[460px] bg-[radial-gradient(60%_100%_at_50%_0%,var(--color-accent),transparent_70%)]"
      />
      <div
        aria-hidden
        className="sh-glow pointer-events-none absolute left-1/2 top-[-160px] h-[560px] w-[920px] -translate-x-1/2 blur-2xl"
        style={{
          background:
            "radial-gradient(50% 50% at 50% 50%, rgb(11 111 206 / 0.18), transparent 70%)",
        }}
      />

      <Container className="relative pt-16 pb-20 sm:pt-20 sm:pb-24 lg:pt-24">
        <div className="mx-auto max-w-3xl text-center">
          <div className="sh-intro sh-intro-1 flex justify-center">
            <Pill>
              <span className="font-mono">{HERO.eyebrow}</span>
            </Pill>
          </div>

          <h1 className="sh-intro sh-intro-2 mt-6 text-balance text-5xl font-semibold leading-[1.05] tracking-tight text-foreground sm:text-6xl lg:text-7xl">
            {HERO.title}
          </h1>

          <p className="sh-intro sh-intro-3 mx-auto mt-6 max-w-2xl text-pretty text-lg leading-relaxed text-muted-foreground">
            {HERO.subtitle}
          </p>

          <div className="sh-intro sh-intro-4 mt-9 flex flex-col items-center justify-center gap-3 sm:flex-row">
            <Button
              size="lg"
              className="h-11 px-5 text-[0.95rem] shadow-sm shadow-primary/20 transition-shadow hover:shadow-md hover:shadow-primary/30"
              nativeButton={false}
              render={<Link href={HERO.primaryCta.href} />}
            >
              {HERO.primaryCta.label}
              <ArrowRight />
            </Button>
            <Button
              size="lg"
              variant="outline"
              className="h-11 px-5 text-[0.95rem]"
              nativeButton={false}
              render={<Link href={HERO.secondaryCta.href} />}
            >
              {HERO.secondaryCta.label}
            </Button>
          </div>

          <p className="sh-intro sh-intro-5 mt-7 font-mono text-xs text-muted-foreground">
            {HERO.note}
          </p>
        </div>

        {/* Product visual — floats up on load, tilts, and flattens on hover. */}
        <div className="sh-intro sh-intro-5 relative mx-auto mt-14 max-w-5xl sm:mt-16">
          <div
            aria-hidden
            className="absolute inset-x-0 -top-6 bottom-0 -z-10 bg-[radial-gradient(50%_60%_at_50%_40%,var(--color-accent),transparent_75%)]"
          />
          <div className="[perspective:1800px]">
            <ProductMock className="origin-top [transform:rotateX(7deg)] transition-transform duration-700 ease-out hover:[transform:rotateX(0deg)] motion-reduce:[transform:none]" />
          </div>
        </div>
      </Container>
    </section>
  );
}
