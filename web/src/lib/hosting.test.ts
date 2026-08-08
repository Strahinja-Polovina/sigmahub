import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { availabilityMatrix, canHost, HOSTS_NOTHING_REASON } from "./hosting";
import type { ResourceKind, ServerType } from "@/lib/mock";

// The web matrix must mirror the CP's server-side enforcement, or the wizard
// offers servers the API then rejects with a 422. Rather than hand-copying the
// table (which drifts the moment someone edits one side), parse the Go source:
// a mismatch fails here instead of in front of a user.
const GO_DOMAIN = readFileSync(
  join(process.cwd(), "..", "cp", "internal", "store", "domain.go"),
  "utf8"
);

/** The web calls MongoDB "mongo"; the control plane calls it "mongodb". */
const KIND_ALIASES: Record<string, ResourceKind> = { mongodb: "mongo" };

function cpMatrix(): Record<string, string[]> {
  const block = GO_DOMAIN.match(
    /var resourceServerTypes = map\[string\]\[\]string\{([\s\S]*?)\n\}/
  );
  if (!block) throw new Error("resourceServerTypes not found in domain.go");
  const out: Record<string, string[]> = {};
  for (const [, kind, list] of block[1].matchAll(/"([a-z0-9]+)":\s*\{([^}]*)\}/g)) {
    const webKind = KIND_ALIASES[kind] ?? kind;
    out[webKind] = [...list.matchAll(/"([a-z0-9]+)"/g)].map((m) => m[1]);
  }
  return out;
}

function cpServerTypes(): string[] {
  const block = GO_DOMAIN.match(/var serverTypes = map\[string\]bool\{([\s\S]*?)\n\}/);
  if (!block) throw new Error("serverTypes not found in domain.go");
  return [...block[1].matchAll(/"([a-z0-9]+)":/g)].map((m) => m[1]);
}

describe("availability matrix", () => {
  it("mirrors the control plane's kind → server-type matrix exactly", () => {
    const cp = cpMatrix();
    for (const [kind, servers] of Object.entries(cp)) {
      for (const server of Object.keys(availabilityMatrix) as ServerType[]) {
        expect({ kind, server, canHost: canHost(server, kind as ResourceKind) }).toEqual({
          kind,
          server,
          canHost: servers.includes(server),
        });
      }
    }
  });

  it("knows every server type the control plane accepts", () => {
    // A type the CP enrolls but the matrix has never heard of would render as a
    // server nothing can be scheduled onto, with no explanation.
    for (const type of cpServerTypes()) {
      expect(Object.keys(availabilityMatrix)).toContain(type);
    }
  });

  it("never lets a database land on a storage or gpu server", () => {
    for (const kind of ["postgres", "mysql", "mongo", "redis"] as ResourceKind[]) {
      expect(canHost("storage", kind)).toBe(false);
      expect(canHost("gpu", kind)).toBe(false);
    }
  });

  it("treats a VPS as a general-purpose host", () => {
    // Virtualization is a disclosure, not a capability difference.
    expect(availabilityMatrix.vps).toEqual(availabilityMatrix.general);
  });

  it("schedules nothing directly onto cluster or build nodes", () => {
    for (const type of ["k8s", "build"] as ServerType[]) {
      expect(availabilityMatrix[type]).toEqual([]);
      // And says why, so the UI never shows an unexplained empty list.
      expect(HOSTS_NOTHING_REASON[type]).toBeTruthy();
    }
  });

  it("only serves models on GPU hardware", () => {
    for (const type of Object.keys(availabilityMatrix) as ServerType[]) {
      expect(canHost(type, "llm")).toBe(type === "gpu");
    }
  });
});
