/**
 * Shared marketing primitives.
 *
 * Every public-site section is built from these so vertical rhythm, container
 * width, type scale and card chrome stay identical across the page. Sections
 * should NOT hand-roll their own section padding, container or heading sizes —
 * always compose from here.
 */

import * as React from "react";

import { cn } from "@/lib/utils";
import { Reveal } from "@/components/marketing/reveal";

/* -------------------------------------------------------------------------- */
/*  Layout                                                                    */
/* -------------------------------------------------------------------------- */

/** Max-width content column with responsive gutters. */
export function Container({
  className,
  children,
}: {
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("mx-auto w-full max-w-6xl px-4 sm:px-6 lg:px-8", className)}>
      {children}
    </div>
  );
}

type SectionProps = {
  id?: string;
  /** Background treatment. `muted` = subtle grey band, `tinted` = faint blue wash. */
  variant?: "default" | "muted" | "tinted";
  /** Draw a hairline divider at the bottom edge. */
  divider?: boolean;
  className?: string;
  containerClassName?: string;
  children: React.ReactNode;
};

/**
 * A full-width page section with the canonical vertical rhythm and container.
 * Pass `id` for in-page anchor navigation.
 */
export function Section({
  id,
  variant = "default",
  divider = true,
  className,
  containerClassName,
  children,
}: SectionProps) {
  return (
    <section
      id={id}
      className={cn(
        "scroll-mt-16 py-20 sm:py-24 lg:py-28",
        variant === "muted" && "bg-muted/40",
        variant === "tinted" && "bg-accent/40",
        divider && "border-b border-border",
        className,
      )}
    >
      <Container className={containerClassName}>
        <Reveal>{children}</Reveal>
      </Container>
    </section>
  );
}

/* -------------------------------------------------------------------------- */
/*  Typographic building blocks                                               */
/* -------------------------------------------------------------------------- */

/** Small mono accent label that sits above a heading. */
export function Eyebrow({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-2 font-mono text-xs font-medium uppercase tracking-[0.14em] text-primary",
        className,
      )}
    >
      <span className="size-1.5 rounded-full bg-primary" aria-hidden />
      {children}
    </span>
  );
}

/**
 * Standard section header: eyebrow + title + optional subtitle.
 * `align` controls text alignment and whether the block is centered + width-capped.
 */
export function SectionHeading({
  eyebrow,
  title,
  subtitle,
  align = "center",
  className,
}: {
  eyebrow?: React.ReactNode;
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  align?: "center" | "left";
  className?: string;
}) {
  return (
    <div
      className={cn(
        align === "center" ? "mx-auto max-w-2xl text-center" : "max-w-2xl",
        className,
      )}
    >
      {eyebrow ? <Eyebrow>{eyebrow}</Eyebrow> : null}
      <h2
        className={cn(
          "text-balance text-3xl font-semibold tracking-tight text-foreground sm:text-4xl",
          eyebrow && "mt-4",
        )}
      >
        {title}
      </h2>
      {subtitle ? (
        <p className="mt-4 text-pretty text-base leading-relaxed text-muted-foreground sm:text-lg">
          {subtitle}
        </p>
      ) : null}
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/*  Small UI atoms                                                            */
/* -------------------------------------------------------------------------- */

/** Rounded outline pill used for hero badges and small status labels. */
export function Pill({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-2 rounded-full border border-border bg-background/80 px-3 py-1 text-xs text-muted-foreground shadow-sm backdrop-blur",
        className,
      )}
    >
      {children}
    </span>
  );
}

/**
 * Decorative dot/grid background. Absolutely positioned; wrap in a relative
 * parent. Fades out toward the edges via a radial mask.
 */
export function GridPattern({
  className,
  variant = "grid",
}: {
  className?: string;
  variant?: "grid" | "dots";
}) {
  const grid =
    "[background-image:linear-gradient(to_right,var(--color-border)_1px,transparent_1px),linear-gradient(to_bottom,var(--color-border)_1px,transparent_1px)] [background-size:56px_56px]";
  const dots =
    "[background-image:radial-gradient(var(--color-border)_1px,transparent_1px)] [background-size:22px_22px]";
  return (
    <div
      aria-hidden="true"
      className={cn(
        "pointer-events-none absolute inset-0 opacity-60 [mask-image:radial-gradient(ellipse_at_center,black,transparent_75%)]",
        variant === "grid" ? grid : dots,
        className,
      )}
    />
  );
}

/** Standard bordered content card with consistent chrome and hover. */
export function Card({
  className,
  children,
  interactive = false,
}: {
  className?: string;
  children: React.ReactNode;
  interactive?: boolean;
}) {
  return (
    <div
      className={cn(
        "rounded-xl border border-border bg-card p-6 shadow-sm",
        interactive &&
          "transition-all duration-200 hover:-translate-y-0.5 hover:border-primary/40 hover:shadow-md hover:shadow-primary/5",
        className,
      )}
    >
      {children}
    </div>
  );
}

/** Square icon chip used across feature cards for a uniform monochrome look. */
export function IconChip({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-grid size-10 shrink-0 place-items-center rounded-lg bg-accent text-primary ring-1 ring-inset ring-primary/15",
        className,
      )}
    >
      {children}
    </span>
  );
}
