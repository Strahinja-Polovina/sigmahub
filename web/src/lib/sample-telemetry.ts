import type { MetricPoint, LogLine } from "@/lib/mock";

// Simulated observability data. v1 has no agent-side metrics/log ingestion yet,
// so resource/server charts and the log viewer render deterministic synthetic
// series (seeded by the resource/server id, no Math.random → stable SSR/CSR).
// Replaced by real telemetry once the agent streams metrics into the platform.

function seedOf(str: string) {
  let h = 0;
  for (const c of str) h = (h * 31 + c.charCodeAt(0)) % 997;
  return h;
}

export function getMetrics(seedKey: string, points = 24): MetricPoint[] {
  const h = seedOf(seedKey);
  const base = (h % 35) + 20;
  return Array.from({ length: points }, (_, i) => ({
    t: `${String(i).padStart(2, "0")}:00`,
    cpu: Math.max(2, Math.round(base + 18 * Math.sin((i + h) / 3) + (i % 5) * 2)),
    mem: Math.max(10, Math.round(base + 12 * Math.cos((i + h) / 4) + 22)),
    net: Math.max(1, Math.round(30 + 25 * Math.sin((i + h) / 2))),
  }));
}

const LOG_MSGS = [
  "GET /health 200 3ms",
  "request completed 200 in 21ms",
  "worker picked up job #{n}",
  "cache hit ratio 0.94",
  "slow query 812ms on orders",
  "reconnecting to upstream",
  "deploy: health check passed",
  "GET /api/v1/orders 200 18ms",
  "background sync ok",
  "rate limit near threshold",
  "container started",
  "memory usage 78%",
];
const LOG_LEVELS: LogLine["level"][] = ["info", "info", "info", "warn", "info", "error", "info"];

export function getLogs(seedKey: string, n = 40): LogLine[] {
  const h = seedOf(seedKey);
  return Array.from({ length: n }, (_, i) => {
    const k = (h + i * 7) % LOG_MSGS.length;
    return {
      t: `12:${String((h + i) % 60).padStart(2, "0")}:${String((i * 13) % 60).padStart(2, "0")}`,
      level: LOG_LEVELS[(h + i) % LOG_LEVELS.length],
      msg: LOG_MSGS[k].replace("{n}", String(1000 + i)),
    };
  });
}
