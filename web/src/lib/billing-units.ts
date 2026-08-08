// Billing units — the read model behind the dashboard's bill.
//
// The weights, the unit price and the free tier are GENERATED from the control
// plane (cp/internal/store/server_catalog.go via server-catalog.generated.ts);
// this module holds only the arithmetic the dashboard does on top of them. It
// used to keep its own copy of the weight table, which is how `vps` and `build`
// came to be missing from both sides at once (SIGMA-198).
//
// A server bills as a number of UNITS whose weight tracks how expensive it is
// to MANAGE, never what the hardware costs — customers always bring their own
// infrastructure and we never mark it up.

import type { ServerType } from "@/lib/mock";
import {
  CURRENCY,
  DEFAULT_UNIT_WEIGHT,
  FREE_TIER_UNITS,
  SERVER_UNIT_WEIGHTS,
  UNIT_PRICE,
  serverUnitWeight,
} from "@/lib/server-catalog.generated";

export {
  CURRENCY,
  DEFAULT_UNIT_WEIGHT,
  FREE_TIER_UNITS,
  SERVER_UNIT_WEIGHTS,
  UNIT_PRICE,
  serverUnitWeight,
};

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
