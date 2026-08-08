import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it, expect } from "vitest";
import {
  SERVER_UNIT_WEIGHTS,
  DEFAULT_UNIT_WEIGHT,
  UNIT_PRICE,
  FREE_TIER_UNITS,
  serverUnitWeight,
  summarizeUnits,
  billableUnits,
} from "./billing-units";
import { SERVER_TYPES } from "./server-catalog.generated";

// The weight table is no longer duplicated: it is a FIELD of each server type in
// the CP catalog, generated into server-catalog.generated.ts, and the staleness
// of that file is guarded in hosting.test.ts. What is left to check here is that
// billing reads the generated table (rather than growing a copy back), that the
// pricing scalars still agree with billing.go, and the arithmetic on top.
//
// This used to regex-parse `var serverUnitWeights` out of server_units.go — a
// guard that proved the two maps matched but could say nothing about the types
// missing from BOTH of them, which is exactly how `vps` and `build` came to bill
// at the fallback weight (SIGMA-198).
const GO_BILLING = readFileSync(
  join(process.cwd(), "..", "cp", "internal", "store", "billing.go"),
  "utf8"
);

function goConst(name: string): number {
  const m = GO_BILLING.match(new RegExp(`${name}\\s*=\\s*(\\d+)`));
  if (!m) throw new Error(`${name} not found in billing.go`);
  return Number(m[1]);
}

describe("billing units come from the control plane's catalog", () => {
  it("prices every server type the control plane knows", () => {
    // A type with no weight bills at the fallback — invisible until an invoice
    // disagrees with the dashboard.
    for (const type of SERVER_TYPES) {
      expect(SERVER_UNIT_WEIGHTS[type], `${type} has no weight`).toBeGreaterThan(0);
    }
    expect(Object.keys(SERVER_UNIT_WEIGHTS).sort()).toEqual([...SERVER_TYPES].sort());
  });

  it("agrees on the unit price and free tier", () => {
    expect(UNIT_PRICE).toBe(goConst("BillingUnitPrice"));
    expect(FREE_TIER_UNITS).toBe(goConst("BillingFreeTier"));
  });

  it("agrees on the default weight for unknown types", () => {
    const GO_UNITS = readFileSync(
      join(process.cwd(), "..", "cp", "internal", "store", "server_units.go"),
      "utf8"
    );
    const m = GO_UNITS.match(/DefaultServerUnitWeight = (\d+)/);
    expect(DEFAULT_UNIT_WEIGHT).toBe(Number(m?.[1]));
  });
});

describe("serverUnitWeight", () => {
  it("weighs a GPU server four times an ordinary one", () => {
    expect(serverUnitWeight("gpu")).toBe(4);
    expect(serverUnitWeight("general")).toBe(1);
  });

  it("bills a VPS and a build server as ordinary servers", () => {
    // Both were absent from the old standalone weight map, so both reached the
    // fallback by accident rather than by decision.
    expect(serverUnitWeight("vps")).toBe(1);
    expect(serverUnitWeight("build")).toBe(1);
  });

  it("bills an unknown type as an ordinary server, never as free", () => {
    // A typo in a type string must not silently zero out a server's bill.
    expect(serverUnitWeight("totally-unknown")).toBe(DEFAULT_UNIT_WEIGHT);
    expect(serverUnitWeight("")).toBe(DEFAULT_UNIT_WEIGHT);
  });
});

describe("summarizeUnits", () => {
  it("prices an all-general fleet exactly as it did before units existed", () => {
    const fleet = [{ type: "general" }, { type: "general" }, { type: "general" }];
    const { servers, units } = summarizeUnits(fleet);
    expect(servers).toBe(3);
    expect(units).toBe(3);
    expect(billableUnits(units)).toBe(0); // still free
  });

  it("weighs a mixed fleet by type and explains the total", () => {
    const { lines, servers, units } = summarizeUnits([
      { type: "general" },
      { type: "general" },
      { type: "gpu" },
    ]);
    expect(servers).toBe(3);
    expect(units).toBe(6); // 1 + 1 + 4
    expect(lines).toEqual([
      { type: "general", count: 2, weight: 1, units: 2 },
      { type: "gpu", count: 1, weight: 4, units: 4 },
    ]);
    expect(billableUnits(units)).toBe(3);
  });

  it("never gives away a lone GPU server", () => {
    // The whole point of the weights: the most valuable use case cannot hide
    // inside the free tier the way a single server would.
    const { units } = summarizeUnits([{ type: "gpu" }]);
    expect(units).toBe(4);
    expect(billableUnits(units)).toBe(1);
  });

  it("keeps a two-node Kubernetes trial cheap", () => {
    const { units } = summarizeUnits([{ type: "k8s" }, { type: "k8s" }]);
    expect(units).toBe(4);
    expect(billableUnits(units) * UNIT_PRICE).toBe(5);
  });

  it("returns no lines and no units for an empty fleet", () => {
    expect(summarizeUnits([])).toEqual({ lines: [], servers: 0, units: 0 });
    expect(billableUnits(0)).toBe(0);
  });
});
