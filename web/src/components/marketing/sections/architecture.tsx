/**
 * Architecture section — a self-contained control-plane / mesh / servers diagram
 * built entirely from design tokens, plus the three "secure by default" cards.
 *
 * Replaces the old architecture-diagram.tsx.
 */

import { ShieldCheck } from "lucide-react";

import { Card, IconChip, Section, SectionHeading } from "@/components/marketing/primitives";
import { ARCHITECTURE } from "@/components/marketing/content";

export function Architecture() {
  const { controlPlane, mesh, servers, security } = ARCHITECTURE;

  return (
    <Section id="architecture" variant="default">
      <SectionHeading
        eyebrow={ARCHITECTURE.eyebrow}
        title={ARCHITECTURE.title}
        subtitle={ARCHITECTURE.subtitle}
        align="center"
      />

      {/* Diagram */}
      <div className="mt-14">
        <Card className="bg-card p-6 sm:p-10">
          <div className="mx-auto flex max-w-3xl flex-col items-center">
            {/* Control plane */}
            <div className="flex w-full max-w-md flex-col items-center rounded-xl border border-primary/25 bg-accent/60 px-6 py-5 text-center shadow-sm">
              <IconChip className="ring-primary/20">
                <ShieldCheck className="size-5" />
              </IconChip>
              <p className="mt-3 text-sm font-semibold tracking-tight text-foreground sm:text-base">
                {controlPlane.title}
              </p>
              <p className="mt-1 font-mono text-[11px] leading-relaxed text-muted-foreground">
                {controlPlane.note}
              </p>
            </div>

            {/* Connector */}
            <span aria-hidden className="h-6 w-px bg-border sm:h-8" />

            {/* WireGuard mesh bar */}
            <div className="flex w-full items-center gap-3 rounded-lg border border-dashed border-primary/40 bg-background px-4 py-3">
              <span aria-hidden className="size-1.5 shrink-0 rounded-full bg-primary" />
              <span className="flex-1 text-center font-mono text-[10px] font-medium uppercase tracking-[0.16em] text-muted-foreground sm:text-[11px]">
                {mesh}
              </span>
              <span aria-hidden className="size-1.5 shrink-0 rounded-full bg-primary" />
            </div>

            {/* Connector */}
            <span aria-hidden className="h-6 w-px bg-border sm:h-8" />

            {/* Typed servers */}
            <div className="grid w-full grid-cols-2 gap-3 sm:grid-cols-4">
              {servers.map((server) => {
                const Icon = server.icon;
                return (
                  <div
                    key={server.label}
                    className="flex flex-col items-center gap-1.5 rounded-lg border border-border bg-background px-3 py-4 text-center shadow-sm"
                  >
                    <Icon className="size-5 text-muted-foreground" aria-hidden />
                    <p className="text-sm font-medium text-foreground">{server.label}</p>
                    <p className="font-mono text-[11px] text-muted-foreground">{server.note}</p>
                  </div>
                );
              })}
            </div>
          </div>

          <p className="mx-auto mt-8 max-w-2xl text-center text-sm leading-relaxed text-muted-foreground">
            Servers you own — connected over an encrypted mesh, orchestrated from one control
            plane. SigmaHub never resells hardware.
          </p>
        </Card>
      </div>

      {/* Security cards */}
      <div className="mt-8 grid grid-cols-1 gap-5 md:grid-cols-3">
        {security.map((item) => {
          const Icon = item.icon;
          return (
            <Card key={item.title}>
              <IconChip>
                <Icon className="size-5" />
              </IconChip>
              <h3 className="mt-4 text-base font-semibold tracking-tight text-foreground">
                {item.title}
              </h3>
              <p className="mt-2 text-sm leading-relaxed text-muted-foreground">{item.body}</p>
            </Card>
          );
        })}
      </div>
    </Section>
  );
}
