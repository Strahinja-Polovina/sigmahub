import Link from "next/link";
import { RotateCcw, Server, ShieldCheck } from "lucide-react";

import { Logo } from "@/components/logo";
import { ThemeToggle } from "@/components/theme-toggle";

const VALUE_BULLETS = [
  {
    icon: Server,
    title: "Your servers, our control plane",
    body: "Deploy apps, databases, object storage and GPU/LLM inference on hardware you own — from one dashboard. We never resell you a server.",
  },
  {
    icon: ShieldCheck,
    title: "Everything included",
    body: "Backups, disaster recovery, Kubernetes and S3 storage come standard. No add-ons, no egress fees, no surprise line items.",
  },
  {
    icon: RotateCcw,
    title: "One simple meter",
    body: "€5 per connected server / month, every feature included. Your first three servers are always free.",
  },
];

const TRUST_STATS = [
  { value: "8", label: "primitives" },
  { value: "1", label: "flat price" },
  { value: "0", label: "servers resold" },
];

export default function AuthLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <div className="grid min-h-screen bg-background lg:grid-cols-2">
      {/* Left: form column */}
      <div className="relative flex flex-col">
        <header className="flex items-center justify-between px-6 py-5 lg:px-10">
          <Link
            href="/"
            className="rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <Logo />
          </Link>
          <ThemeToggle />
        </header>

        <main className="flex flex-1 items-center justify-center px-6 pb-16">
          <div className="w-full max-w-sm">{children}</div>
        </main>

        <footer className="px-6 py-6 text-center text-xs text-muted-foreground lg:px-10 lg:text-left">
          © {new Date().getFullYear()} SigmaHub. Managed PaaS for your own
          servers.
        </footer>
      </div>

      {/* Right: product panel (hidden on mobile) */}
      <aside className="relative hidden overflow-hidden border-l border-border bg-muted/40 lg:flex lg:flex-col lg:justify-center">
        {/* Faint grid + a soft blue wash rising from the top, mirroring the marketing hero. */}
        <div
          aria-hidden="true"
          className="pointer-events-none absolute inset-0 [background-image:linear-gradient(to_right,var(--color-border)_1px,transparent_1px),linear-gradient(to_bottom,var(--color-border)_1px,transparent_1px)] [background-size:44px_44px] opacity-40 [mask-image:radial-gradient(ellipse_at_center,black,transparent_75%)]"
        />
        <div
          aria-hidden="true"
          className="pointer-events-none absolute inset-x-0 top-0 h-[380px] bg-[radial-gradient(70%_100%_at_50%_0%,var(--color-accent),transparent_70%)]"
        />

        <div className="relative mx-auto w-full max-w-md px-12">
          <Logo className="scale-110 origin-left" />
          <h2 className="mt-8 text-balance text-2xl font-semibold tracking-tight text-foreground">
            The cloud console for servers you already own.
          </h2>
          <p className="mt-3 text-pretty text-sm leading-relaxed text-muted-foreground">
            SigmaHub gives your own machines a managed control plane — without
            reselling you a single server.
          </p>

          <ul className="mt-10 grid gap-6">
            {VALUE_BULLETS.map(({ icon: Icon, title, body }) => (
              <li key={title} className="flex gap-3.5">
                <span className="mt-0.5 inline-grid size-9 shrink-0 place-items-center rounded-lg bg-accent text-primary ring-1 ring-inset ring-primary/15">
                  <Icon className="size-4.5" />
                </span>
                <div>
                  <p className="text-sm font-medium text-foreground">{title}</p>
                  <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
                    {body}
                  </p>
                </div>
              </li>
            ))}
          </ul>

          {/* Trust element: a compact, factual stat row — no testimonials, no names. */}
          <div className="mt-12 flex items-stretch rounded-xl border border-border bg-card px-2 py-4 shadow-sm">
            {TRUST_STATS.map(({ value, label }, i) => (
              <div
                key={label}
                className={
                  "flex flex-1 flex-col items-center gap-1 px-2 text-center" +
                  (i > 0 ? " border-l border-border" : "")
                }
              >
                <span className="font-mono text-xl font-semibold tracking-tight text-foreground">
                  {value}
                </span>
                <span className="text-[0.7rem] uppercase tracking-[0.12em] text-muted-foreground">
                  {label}
                </span>
              </div>
            ))}
          </div>
        </div>
      </aside>
    </div>
  );
}
