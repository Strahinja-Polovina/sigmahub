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

// The weight table is duplicated across two codebases on purpose (the CP bills,
// the dashboard explains the bill). These tests parse the Go source so a weight
// changed on one side and not the other fails here rather than in production —
// the same guard the hosting matrix uses.
const GO_UNITS = readFileSync(
  join(process.cwd(), "..", "cp", "internal", "store", "server_units.go"),
  "utf8"
);
const GO_BILLING = readFileSync(
  join(process.cwd(), "..", "cp", "internal", "store", "billing.go"),
  "utf8"
);

function goWeights(): Record<string, number> {
  const block = GO_UNITS.match(
    /var serverUnitWeights = map\[string\]int\{([\s\S]*?)\n\}/
  );
  if (!block) throw new Error("serverUnitWeights not found in server_units.go");
  const out: Record<string, number> = {};
  for (const [, type, weight] of block[1].matchAll(/"([a-z0-9_]+)":\s*(\d+)/g)) {
    out[type] = Number(weight);
  }
  return out;
}

function goConst(name: string): number {
  const m = GO_BILLING.match(new RegExp(`${name}\\s*=\\s*(\\d+)`));
  if (!m) throw new Error(`${name} not found in billing.go`);
  return Number(m[1]);
}

describe("billing units mirror the control plane", () => {
  it("has the same weight for every server type", () => {
    expect(SERVER_UNIT_WEIGHTS).toEqual(goWeights());
  });

  it("agrees on the unit price and free tier", () => {
    expect(UNIT_PRICE).toBe(goConst("BillingUnitPrice"));
    expect(FREE_TIER_UNITS).toBe(goConst("BillingFreeTier"));
  });

  it("agrees on the default weight for unknown types", () => {
    const m = GO_UNITS.match(/DefaultServerUnitWeight = (\d+)/);
    expect(DEFAULT_UNIT_WEIGHT).toBe(Number(m?.[1]));
  });
});

describe("serverUnitWeight", () => {
  it("weighs a GPU server four times an ordinary one", () => {
    expect(serverUnitWeight("gpu")).toBe(4);
    expect(serverUnitWeight("general")).toBe(1);
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
