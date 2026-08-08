// Billing units — the web mirror of the CP's weight table
// (cp/internal/store/server_units.go). A test asserts the two agree, the same
// way the hosting matrix is kept honest across the two codebases.
//
// A server bills as a number of UNITS whose weight tracks how expensive it is
// to MANAGE, never what the hardware costs — customers always bring their own
// infrastructure and we never mark it up.

import type { ServerType } from "@/lib/mock";

/** Price of one unit per month. */
export const UNIT_PRICE = 5;
/** Units included for free. Three ordinary servers still cost nothing. */
export const FREE_TIER_UNITS = 3;
export const CURRENCY = "EUR";

/** Weight of an unknown/legacy type: bill as an ordinary server, never zero. */
export const DEFAULT_UNIT_WEIGHT = 1;

export const SERVER_UNIT_WEIGHTS: Record<string, number> = {
  general: 1,
  database: 1,
  storage: 1,
  k8s: 2,
  gpu: 4,
};

export function serverUnitWeight(serverType: string): number {
  return SERVER_UNIT_WEIGHTS[serverType] ?? DEFAULT_UNIT_WEIGHT;
}

/** One line of the billing breakdown: what a server type contributes. */
export type ServerUnitLine = {
  type: string;
  count: number;
  weight: number;
  units: number;
};

/**
 * Summarize a fleet into billing units. Mirrors the CP's ConnectedServerUnits:
 * lines sorted by type, the plain server count, and the weighted total.
 */
export function summarizeUnits(
  servers: { type: ServerType | string }[]
): { lines: ServerUnitLine[]; servers: number; units: number } {
  const counts = new Map<string, number>();
  for (const sv of servers) {
    counts.set(sv.type, (counts.get(sv.type) ?? 0) + 1);
  }
  const lines: ServerUnitLine[] = [...counts.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([type, count]) => {
      const weight = serverUnitWeight(type);
      return { type, count, weight, units: count * weight };
    });
  return {
    lines,
    servers: servers.length,
    units: lines.reduce((sum, l) => sum + l.units, 0),
  };
}

/** Units actually charged for, after the free tier. */
export function billableUnits(units: number): number {
  return Math.max(0, units - FREE_TIER_UNITS);
}

/** Human label for a unit weight, e.g. "GPU server · 4 units". */
export function unitsLabel(units: number): string {
  return `${units} ${units === 1 ? "unit" : "units"}`;
}
