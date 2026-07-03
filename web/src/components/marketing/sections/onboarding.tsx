import Link from "next/link";
import { ArrowRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Eyebrow, Section } from "@/components/marketing/primitives";
import { ONBOARDING } from "@/components/marketing/content";

export function Onboarding() {
  return (
    <Section id="docs" variant="default">
      <div className="grid items-center gap-10 lg:grid-cols-2">
        {/* Left: copy + CTA */}
        <div>
          <Eyebrow>{ONBOARDING.eyebrow}</Eyebrow>
          <h2 className="mt-4 text-balance text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            {ONBOARDING.title}
          </h2>
          <p className="mt-4 text-pretty text-base leading-relaxed text-muted-foreground sm:text-lg">
            {ONBOARDING.subtitle}
          </p>
          <div className="mt-8">
            <Button
              size="lg"
              variant="outline"
              className="h-11 px-5 text-[0.95rem]"
              nativeButton={false}
              render={<Link href={ONBOARDING.cta.href} />}
            >
              {ONBOARDING.cta.label}
              <ArrowRight />
            </Button>
          </div>
        </div>

        {/* Right: terminal block (the one permitted dark block) */}
        <div className="rounded-xl border border-slate-800 bg-slate-950 shadow-lg">
          {/* Top bar */}
          <div className="flex items-center gap-2 border-b border-slate-800 px-4 py-2.5">
            <div className="flex gap-1.5" aria-hidden>
              <span className="size-2.5 rounded-full bg-slate-700" />
              <span className="size-2.5 rounded-full bg-slate-700" />
              <span className="size-2.5 rounded-full bg-slate-700" />
            </div>
            <span className="mx-auto font-mono text-xs text-slate-500">
              {ONBOARDING.terminalTitle}
            </span>
          </div>

          {/* Body */}
          <div className="overflow-x-auto p-5">
            <pre className="font-mono text-sm leading-relaxed">
              <code>
                {ONBOARDING.lines.map((line) => (
                  <span key={line.command} className="block whitespace-pre">
                    <span className="select-none text-slate-500">
                      {line.prompt}{" "}
                    </span>
                    <span className="text-slate-100">{line.command}</span>
                  </span>
                ))}
                <span className="mt-3 block whitespace-pre text-emerald-400">
                  {ONBOARDING.success}
                </span>
              </code>
            </pre>
          </div>
        </div>
      </div>
    </Section>
  );
}
