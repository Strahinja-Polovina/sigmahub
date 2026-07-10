"use client";

import * as React from "react";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";

import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart";
import { getMetrics } from "@/lib/sample-telemetry";

// Demo mode charts simulated cpu/mem/net; CP mode charts the real agent
// samples, whose third series is disk usage.
const simConfig = {
  cpu: { label: "CPU %", color: "var(--chart-1)" },
  mem: { label: "Memory %", color: "var(--chart-2)" },
  net: { label: "Network Mb/s", color: "var(--chart-3)" },
} satisfies ChartConfig;

const cpConfig = {
  cpu: { label: "CPU %", color: "var(--chart-1)" },
  mem: { label: "Memory %", color: "var(--chart-2)" },
  disk: { label: "Disk %", color: "var(--chart-3)" },
} satisfies ChartConfig;

/** One real sample from the control plane, pre-shaped for the chart. */
export type MetricsPoint = { t: string; cpu: number; mem: number; disk: number };

type Metric = string;

export function ServerMetrics({
  seedKey,
  points,
}: {
  seedKey: string;
  points?: MetricsPoint[];
}) {
  const real = points !== undefined;
  const chartConfig: ChartConfig = real ? cpConfig : simConfig;
  const [metric, setMetric] = React.useState<Metric>("cpu");
  const sim = React.useMemo(
    () => (real ? [] : getMetrics(seedKey)),
    [real, seedKey]
  );
  const data: { t: string; [series: string]: string | number }[] = real
    ? points
    : sim.map((p) => ({ ...p }));

  if (real && data.length === 0) {
    return (
      <div className="flex flex-col items-center gap-2 py-12 text-center">
        <p className="text-sm font-medium text-foreground">No samples yet</p>
        <p className="max-w-sm text-xs text-muted-foreground">
          Metrics appear after the agent&apos;s next heartbeats.
        </p>
      </div>
    );
  }

  const latest = data[data.length - 1];
  const color = chartConfig[metric].color;
  const gradientId = `fill-${metric}`;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        {(Object.keys(chartConfig) as Metric[]).map((m) => {
          const active = m === metric;
          return (
            <button
              key={m}
              type="button"
              onClick={() => setMetric(m)}
              className={`flex flex-col items-start gap-0.5 rounded-lg border px-3 py-2 text-left transition-colors ${
                active
                  ? "border-border bg-muted/60"
                  : "border-transparent hover:bg-muted/40"
              }`}
            >
              <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                <span
                  className="size-2 rounded-full"
                  style={{ backgroundColor: chartConfig[m].color }}
                  aria-hidden
                />
                {chartConfig[m].label}
              </span>
              <span className="text-base font-semibold text-foreground tabular-nums">
                {latest?.[m]}
              </span>
            </button>
          );
        })}
      </div>

      <ChartContainer config={chartConfig} className="aspect-auto h-52 w-full">
        <AreaChart data={data} margin={{ left: 4, right: 8, top: 4 }}>
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="5%" stopColor={color} stopOpacity={0.3} />
              <stop offset="95%" stopColor={color} stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid vertical={false} strokeDasharray="3 3" />
          <XAxis
            dataKey="t"
            tickLine={false}
            axisLine={false}
            tickMargin={8}
            minTickGap={24}
            interval="preserveStartEnd"
          />
          <YAxis
            tickLine={false}
            axisLine={false}
            width={28}
            tickMargin={4}
          />
          <ChartTooltip
            cursor={false}
            content={<ChartTooltipContent indicator="line" />}
          />
          <Area
            dataKey={metric}
            type="monotone"
            stroke={color}
            strokeWidth={2}
            fill={`url(#${gradientId})`}
            isAnimationActive={false}
          />
        </AreaChart>
      </ChartContainer>
    </div>
  );
}
