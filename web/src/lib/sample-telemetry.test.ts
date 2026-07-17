import { describe, expect, it } from "vitest";
import { getMetrics, getLogs } from "./sample-telemetry";

// The demo-mode series must be deterministic per seed (stable SSR/CSR — a
// hydration mismatch here breaks every resource page in demo mode).
describe("sample telemetry determinism", () => {
  it("returns identical series for the same seed", () => {
    expect(getMetrics("res_abc")).toEqual(getMetrics("res_abc"));
    expect(getLogs("res_abc")).toEqual(getLogs("res_abc"));
  });
  it("varies by seed", () => {
    expect(getMetrics("res_abc")).not.toEqual(getMetrics("res_xyz"));
  });
  it("keeps values in sane display ranges", () => {
    for (const p of getMetrics("res_abc")) {
      expect(p.cpu).toBeGreaterThanOrEqual(0);
      expect(p.mem).toBeGreaterThanOrEqual(0);
      expect(p.net).toBeGreaterThanOrEqual(0);
    }
  });
});
