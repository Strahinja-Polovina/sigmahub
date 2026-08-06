"use client";

import * as React from "react";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";

import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart";
import type { MetricPoint } from "@/lib/mock";

type MetricKey = "cpu" | "mem" | "net";

// CPU and network are percentages/rates that belong on a fixed 0–100 axis;
// memory is a percentage in demo mode but absolute MiB in CP mode. Plotting a
// MiB value (e.g. 512) on the 0–100 axis silently rescales it and squashes the
// CPU series into an unreadable band (SIGMA-150). When `memUnit` is "MiB" the
// mem series gets its own right-hand axis and a MiB-labeled config.
export function MetricsChart({
  data,
  keys = ["cpu", "mem", "net"],
  className,
  memUnit = "%",
}: {
  data: MetricPoint[];
  keys?: MetricKey[];
  className?: string;
  memUnit?: "%" | "MiB";
}) {
  const memIsAbsolute = memUnit === "MiB";
  const chartConfig = {
    cpu: { label: "CPU %", color: "var(--chart-1)" },
    mem: { label: memIsAbsolute ? "Memory MiB" : "Memory %", color: "var(--chart-2)" },
    net: { label: "Network Mb/s", color: "var(--chart-3)" },
  } satisfies ChartConfig;

  // mem rides the right (absolute) axis only when it is MiB; everything else,
  // and mem-as-% in demo mode, stays on the left 0–100 axis.
  const axisFor = (k: MetricKey) => (memIsAbsolute && k === "mem" ? "mem" : "pct");

  return (
    <ChartContainer config={chartConfig} className={className}>
      <AreaChart data={data} margin={{ left: 4, right: 8, top: 8, bottom: 0 }}>
        <defs>
          {keys.map((k) => (
            <linearGradient
              key={k}
              id={`fill-${k}`}
              x1="0"
              y1="0"
              x2="0"
              y2="1"
            >
              <stop offset="5%" stopColor={`var(--color-${k})`} stopOpacity={0.3} />
              <stop offset="95%" stopColor={`var(--color-${k})`} stopOpacity={0.03} />
            </linearGradient>
          ))}
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
          yAxisId="pct"
          tickLine={false}
          axisLine={false}
          tickMargin={8}
          width={28}
          domain={[0, 100]}
          ticks={[0, 50, 100]}
        />
        {memIsAbsolute && keys.includes("mem") && (
          <YAxis
            yAxisId="mem"
            orientation="right"
            tickLine={false}
            axisLine={false}
            tickMargin={8}
            width={40}
            domain={[0, "auto"]}
          />
        )}
        <ChartTooltip cursor content={<ChartTooltipContent indicator="line" />} />
        {keys.map((k) => (
          <Area
            key={k}
            yAxisId={axisFor(k)}
            dataKey={k}
            type="monotone"
            stroke={`var(--color-${k})`}
            strokeWidth={2}
            fill={`url(#fill-${k})`}
            stackId={undefined}
            dot={false}
          />
        ))}
      </AreaChart>
    </ChartContainer>
  );
}
