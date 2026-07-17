import { describe, expect, it } from "vitest";
import { availabilityMatrix, canHost } from "./hosting";
import type { ResourceKind, ServerType } from "@/lib/mock";

// The web matrix must mirror the CP's server-side enforcement
// (cp/internal/store/domain.go resourceServerTypes) — the UI filter and the
// 422 the API returns have to agree, or the wizard offers servers the CP
// rejects.
const CP_MATRIX: Record<ResourceKind, ServerType[]> = {
  app: ["general", "gpu"],
  postgres: ["database", "general"],
  mysql: ["database", "general"],
  mongo: ["database", "general"],
  redis: ["database", "general"],
  s3: ["storage"],
  llm: ["gpu"],
};

describe("availability matrix", () => {
  it("mirrors the control plane's kind → server-type matrix exactly", () => {
    for (const [kind, servers] of Object.entries(CP_MATRIX) as [ResourceKind, ServerType[]][]) {
      for (const server of Object.keys(availabilityMatrix) as ServerType[]) {
        expect({ kind, server, canHost: canHost(server, kind) }).toEqual({
          kind,
          server,
          canHost: servers.includes(server),
        });
      }
    }
  });

  it("never lets a database land on a storage or gpu server", () => {
    for (const kind of ["postgres", "mysql", "mongo", "redis"] as ResourceKind[]) {
      expect(canHost("storage", kind)).toBe(false);
      expect(canHost("gpu", kind)).toBe(false);
    }
  });
});
