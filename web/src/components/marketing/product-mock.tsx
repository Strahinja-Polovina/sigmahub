/**
 * Crafted product mock used as the hero visual — a stylized SigmaHub console
 * that mirrors the real dashboard. Pure presentational markup, themed with the
 * design tokens so it adapts to light and dark automatically.
 */

import {
  Boxes,
  Cpu,
  Database,
  GaugeCircle,
  GitBranch,
  HardDrive,
  LayoutGrid,
  Search,
  Server,
} from "lucide-react";

import { cn } from "@/lib/utils";

const NAV = [
  { icon: LayoutGrid, label: "Overview", active: true },
  { icon: Boxes, label: "Projects", active: false },
  { icon: Server, label: "Servers", active: false },
  { icon: Database, label: "Resources", active: false },
  { icon: GaugeCircle, label: "Billing", active: false },
];

const STATS = [
  { label: "Connected servers", value: "8", delta: "+2" },
  { label: "Monthly cost", value: "€40", delta: "flat" },
  { label: "Deploys / 7d", value: "24", delta: "+6" },
  { label: "Fleet uptime", value: "99.98%", delta: "30d" },
];

const RESOURCES = [
  { icon: GitBranch, name: "webshop", meta: "app · fra-1", status: "Running", tone: "ok" },
  { icon: Cpu, name: "llama-3-70b", meta: "gpu · fsn-1", status: "Serving", tone: "ok" },
  { icon: Database, name: "postgres-prod", meta: "db · fsn-1", status: "Healthy", tone: "ok" },
  { icon: HardDrive, name: "assets", meta: "s3 · hel-1", status: "Syncing", tone: "warn" },
];

// A gentle upward area-chart path (viewBox 0 0 320 96).
const AREA_LINE =
  "M0,74 C36,70 52,58 80,54 C108,50 120,60 152,50 C184,40 196,30 232,28 C268,26 288,18 320,12";
const AREA_FILL = `${AREA_LINE} L320,96 L0,96 Z`;

export function ProductMock({ className }: { className?: string }) {
  return (
    <div
      className={cn(
        "overflow-hidden rounded-xl border border-border bg-card shadow-2xl shadow-foreground/10 ring-1 ring-foreground/5",
        className,
      )}
    >
      {/* Browser chrome */}
      <div className="flex items-center gap-2 border-b border-border bg-muted/50 px-4 py-2.5">
        <div className="flex gap-1.5">
          <span className="size-2.5 rounded-full bg-border" />
          <span className="size-2.5 rounded-full bg-border" />
          <span className="size-2.5 rounded-full bg-border" />
        </div>
        <div className="mx-auto flex items-center gap-1.5 rounded-md border border-border bg-background px-3 py-1 font-mono text-[11px] text-muted-foreground">
          <span className="size-1.5 rounded-full bg-primary" />
          app.sigmahub.io/acme
        </div>
      </div>

      {/* App body */}
      <div className="flex min-h-[320px]">
        {/* Sidebar */}
        <aside className="hidden w-44 shrink-0 flex-col gap-0.5 border-r border-border bg-sidebar/60 p-3 sm:flex">
          <div className="mb-3 flex items-center gap-2 px-1.5">
            <span className="grid size-5 place-items-center rounded bg-primary font-mono text-[11px] font-bold text-primary-foreground">
              Σ
            </span>
            <span className="text-[13px] font-semibold tracking-tight text-foreground">
              SigmaHub
            </span>
          </div>
          {NAV.map((item) => (
            <div
              key={item.label}
              className={cn(
                "flex items-center gap-2 rounded-md px-2 py-1.5 text-[12px] font-medium",
                item.active
                  ? "bg-accent text-primary"
                  : "text-muted-foreground",
              )}
            >
              <item.icon className="size-3.5" />
              {item.label}
            </div>
          ))}
          <div className="mt-auto flex items-center gap-2 rounded-md border border-border bg-background px-2 py-1.5">
            <span className="grid size-5 place-items-center rounded-full bg-accent text-[9px] font-semibold text-primary">
              AC
            </span>
            <span className="text-[11px] font-medium text-foreground">Acme GmbH</span>
          </div>
        </aside>

        {/* Main */}
        <div className="flex-1 p-4 sm:p-5">
          {/* Top bar */}
          <div className="flex items-center justify-between gap-3">
            <div>
              <p className="text-[13px] font-semibold text-foreground">Overview</p>
              <p className="text-[11px] text-muted-foreground">production · 8 servers</p>
            </div>
            <div className="flex items-center gap-2">
              <span className="hidden items-center gap-1.5 rounded-md border border-border bg-background px-2 py-1 font-mono text-[10px] text-muted-foreground md:inline-flex">
                <Search className="size-3" /> ⌘K
              </span>
              <span className="inline-flex items-center gap-1.5 rounded-md bg-accent px-2 py-1 text-[10px] font-medium text-primary">
                <span className="sh-pulse size-1.5 rounded-full bg-primary" />
                All systems operational
              </span>
            </div>
          </div>

          {/* Stat cards */}
          <div className="mt-4 grid grid-cols-2 gap-2.5 lg:grid-cols-4">
            {STATS.map((s) => (
              <div key={s.label} className="rounded-lg border border-border bg-background p-2.5">
                <p className="truncate text-[10px] text-muted-foreground">{s.label}</p>
                <div className="mt-1 flex items-baseline gap-1.5">
                  <span className="text-lg font-semibold tracking-tight text-foreground">
                    {s.value}
                  </span>
                  <span className="text-[10px] font-medium text-primary">{s.delta}</span>
                </div>
              </div>
            ))}
          </div>

          {/* Chart + resources */}
          <div className="mt-3 grid gap-3 lg:grid-cols-5">
            <div className="rounded-lg border border-border bg-background p-3 lg:col-span-3">
              <div className="flex items-center justify-between">
                <p className="text-[11px] font-medium text-foreground">Deploys & requests</p>
                <p className="font-mono text-[10px] text-muted-foreground">7d</p>
              </div>
              <svg
                viewBox="0 0 320 96"
                preserveAspectRatio="none"
                className="mt-2 h-20 w-full"
              >
                <defs>
                  <linearGradient id="mockArea" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--color-primary)" stopOpacity="0.22" />
                    <stop offset="100%" stopColor="var(--color-primary)" stopOpacity="0" />
                  </linearGradient>
                </defs>
                <path d={AREA_FILL} fill="url(#mockArea)" />
                <path
                  className="sh-draw"
                  d={AREA_LINE}
                  pathLength={640}
                  fill="none"
                  stroke="var(--color-primary)"
                  strokeWidth="2"
                  vectorEffect="non-scaling-stroke"
                />
              </svg>
            </div>

            <div className="rounded-lg border border-border bg-background p-2 lg:col-span-2">
              {RESOURCES.map((r, i) => (
                <div
                  key={r.name}
                  className={cn(
                    "flex items-center gap-2 px-1.5 py-1.5",
                    i !== RESOURCES.length - 1 && "border-b border-border/70",
                  )}
                >
                  <span className="grid size-6 place-items-center rounded-md bg-accent text-primary">
                    <r.icon className="size-3" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-[11px] font-medium text-foreground">{r.name}</p>
                    <p className="truncate font-mono text-[9px] text-muted-foreground">{r.meta}</p>
                  </div>
                  <span
                    className={cn(
                      "inline-flex items-center gap-1 text-[9px] font-medium",
                      r.tone === "ok" ? "text-primary" : "text-amber-500",
                    )}
                  >
                    <span
                      className={cn(
                        "size-1.5 rounded-full",
                        r.tone === "ok" ? "bg-primary" : "bg-amber-500",
                      )}
                    />
                    {r.status}
                  </span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
