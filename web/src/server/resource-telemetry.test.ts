import { beforeEach, describe, expect, it, vi } from "vitest";

const cpEnabled = vi.fn(() => true);
const cpQueryResourceMetrics = vi.fn();
const cpQueryLogs = vi.fn();

vi.mock("./cp", () => ({
  cpEnabled: () => cpEnabled(),
  cpQueryResourceMetrics: (...a: unknown[]) => cpQueryResourceMetrics(...a),
  cpQueryLogs: (...a: unknown[]) => cpQueryLogs(...a),
}));

import { loadResourceTelemetry } from "./resource-telemetry";

beforeEach(() => {
  vi.clearAllMocks();
  cpEnabled.mockReturnValue(true);
});

describe("loadResourceTelemetry", () => {
  it("reports a control-plane failure as a load failure, not as an empty pipeline", async () => {
    cpQueryResourceMetrics.mockResolvedValue([]);
    cpQueryLogs.mockRejectedValue(new Error("connect ECONNREFUSED"));

    const failures: string[] = [];
    const telemetry = await loadResourceTelemetry("org_1", "res_1", failures);

    // The whole point: the page must be able to SAY it could not ask. Claiming
    // `pipeline: true` with empty series is the page asserting the pipeline is
    // configured and produced nothing (SIGMA-236).
    expect(failures).toContain("logs and metrics");
    expect(telemetry?.unreadable).toBe(true);
  });

  it("an empty but reachable pipeline is not a failure", async () => {
    cpQueryResourceMetrics.mockResolvedValue([]);
    cpQueryLogs.mockResolvedValue([]);

    const failures: string[] = [];
    const telemetry = await loadResourceTelemetry("org_1", "res_1", failures);

    expect(failures).toEqual([]);
    expect(telemetry).toEqual({ pipeline: true, metrics: [], logs: [] });
  });

  it("a pipeline that is not configured answers null and is not a failure", async () => {
    cpQueryResourceMetrics.mockResolvedValue(null);
    cpQueryLogs.mockResolvedValue(null);

    const failures: string[] = [];
    const telemetry = await loadResourceTelemetry("org_1", "res_1", failures);

    expect(failures).toEqual([]);
    expect(telemetry?.pipeline).toBe(false);
  });

  it("demo mode has no control plane to ask", async () => {
    cpEnabled.mockReturnValue(false);
    const failures: string[] = [];
    expect(await loadResourceTelemetry("org_1", "res_1", failures)).toBeNull();
    expect(failures).toEqual([]);
  });
});
