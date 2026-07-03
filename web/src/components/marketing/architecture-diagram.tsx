import { Cpu, Database, HardDrive, Server, ShieldCheck } from "lucide-react";

const SERVERS = [
  { label: "General", icon: Server, note: "apps · k8s" },
  { label: "Storage", icon: HardDrive, note: "S3 · backups" },
  { label: "Database", icon: Database, note: "postgres · redis" },
  { label: "GPU", icon: Cpu, note: "LLM · inference" },
] as const;

export function ArchitectureDiagram() {
  return (
    <div className="rounded-xl border border-border bg-card p-6 sm:p-10">
      {/* Control plane */}
      <div className="mx-auto flex max-w-md flex-col items-center gap-3 rounded-lg border border-primary/25 bg-accent/60 px-6 py-5 text-center">
        <div className="flex items-center gap-2">
          <ShieldCheck className="size-4 text-primary" />
          <span className="text-sm font-semibold text-foreground">
            SigmaHub control plane
          </span>
        </div>
        <p className="text-xs text-muted-foreground">
          Orchestration, scheduling, metering &amp; observability
        </p>
      </div>

      {/* Connector: control plane -> mesh */}
      <div className="mx-auto h-8 w-px bg-border" aria-hidden />

      {/* WireGuard mesh */}
      <div className="mx-auto flex max-w-lg items-center justify-center gap-2 rounded-lg border border-dashed border-border bg-muted/50 px-5 py-3">
        <span className="size-1.5 rounded-full bg-primary" aria-hidden />
        <span className="font-mono text-xs uppercase tracking-wider text-muted-foreground">
          WireGuard encrypted mesh
        </span>
        <span className="size-1.5 rounded-full bg-primary" aria-hidden />
      </div>

      {/* Connector: mesh -> servers */}
      <div className="mx-auto h-8 w-px bg-border" aria-hidden />

      {/* Typed servers */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {SERVERS.map((s) => (
          <div
            key={s.label}
            className="flex flex-col items-center gap-2 rounded-lg border border-border bg-background px-3 py-4 text-center"
          >
            <s.icon className="size-5 text-muted-foreground" />
            <span className="text-sm font-medium text-foreground">
              {s.label}
            </span>
            <span className="font-mono text-[11px] text-muted-foreground">
              {s.note}
            </span>
          </div>
        ))}
      </div>

      <p className="mt-6 text-center text-xs text-muted-foreground">
        Servers you own — connected over an encrypted mesh, orchestrated from one
        control plane. SigmaHub never resells hardware.
      </p>
    </div>
  );
}
