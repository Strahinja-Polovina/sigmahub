import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  checkServerCompatibility,
  isIncompatible,
  nextServerStatus,
  statusAfterTypeChange,
  SERVER_STATUS,
  type FailedRequirement,
  type HostFacts,
} from "./server-compat";
import { SERVER_TYPES } from "./server-catalog.generated";

// The TypeScript half of the cross-language parity guard.
//
// The control plane owns the compatibility decision; this module re-implements
// it only so demo mode — which has no control plane and no wrong hardware to
// plug in — can show an incompatible server at all. Two implementations of one
// rule drift, and the drift is invisible from either side: the demo would
// refuse hosts the real gate accepts, or word the same refusal differently.
//
// cp/internal/store/testdata/compat_cases.json is the referee. Every case there
// is asserted here and in cp/internal/store/compat_fixture_test.go, sentence
// included, so a reworded explanation fails one suite until the other agrees.

const FIXTURES = join(process.cwd(), "..", "cp", "internal", "store", "testdata", "compat_cases.json");

type Fixture = {
  cases: { name: string; type: string; facts: HostFacts; expect: FailedRequirement[] }[];
};

const fixture = JSON.parse(readFileSync(FIXTURES, "utf8")) as Fixture;

describe("the demo gate agrees with the control plane's", () => {
  it("has fixtures to check at all", () => {
    // A missing or emptied fixture file would make every case below vacuous.
    expect(fixture.cases.length).toBeGreaterThan(5);
  });

  for (const tc of fixture.cases) {
    it(tc.name, () => {
      expect(checkServerCompatibility(tc.type, tc.facts)).toEqual(tc.expect);
    });
  }
});

describe("an unreported fact is unknown, not a violation", () => {
  // The rule that decides whether this is a gate or a fleet-wide outage: on the
  // day it ships, most agents in a customer's fleet predate these facts.
  const compatible: HostFacts = {
    arch: "amd64",
    distro: "ubuntu-24.04",
    diskTotalBytes: 2_000_000_000_000,
    gpu: { vendor: "nvidia", count: 1, driverVersion: "550.54.15" },
  };

  it("accepts a host that meets everything", () => {
    expect(checkServerCompatibility("gpu", compatible)).toEqual([]);
  });

  const blanked: [string, HostFacts][] = [
    ["no distro reported", { ...compatible, distro: undefined }],
    ["no arch reported", { ...compatible, arch: undefined }],
    ["no disk reported", { ...compatible, diskTotalBytes: undefined }],
    ["never looked for a GPU", { ...compatible, gpu: undefined }],
    ["reports nothing at all", {}],
    ["no facts row at all", null as unknown as HostFacts],
  ];
  for (const [name, facts] of blanked) {
    it(`${name} fails nothing, for any type`, () => {
      for (const type of SERVER_TYPES) {
        expect(checkServerCompatibility(type, facts), type).toEqual([]);
      }
    });
  }

  it("still acts on an explicit empty GPU inventory", () => {
    // "I looked and found nothing" is a real reading — it is the whole reason
    // the agent sends this key even on a host with no accelerator.
    const looked = checkServerCompatibility("gpu", { ...compatible, gpu: { vendor: "", count: 0 } });
    expect(looked.map((f) => f.id)).toEqual(["gpu"]);
  });
});

describe("status vocabulary", () => {
  it("keeps incompatible distinct from provisioning and running", () => {
    // The state exists so the UI can offer the two exits. Folding it into
    // either neighbour is what it was invented to avoid: a spinner that never
    // resolves, or a billed server nothing can run on.
    expect(isIncompatible(SERVER_STATUS.incompatible)).toBe(true);
    for (const other of [SERVER_STATUS.provisioning, SERVER_STATUS.running, SERVER_STATUS.unreachable]) {
      expect(isIncompatible(other)).toBe(false);
    }
  });
});

describe("the status machine", () => {
  // The same table as cp/internal/store/compat_test.go's, because demo mode
  // must move a server through the same states the control plane does — a demo
  // that "recovered" a host the real gate would still refuse teaches the wrong
  // thing.
  const refused: FailedRequirement[] = [
    { id: "gpu", fact: "gpu", expected: "", detected: "none", reason: "…" },
  ];
  const cases: [string, string, FailedRequirement[], boolean, string][] = [
    ["refused at registration", SERVER_STATUS.provisioning, refused, false, SERVER_STATUS.incompatible],
    ["refused on a check-in", SERVER_STATUS.running, refused, true, SERVER_STATUS.incompatible],
    ["still refused", SERVER_STATUS.incompatible, refused, true, SERVER_STATUS.incompatible],
    ["first check-in", SERVER_STATUS.provisioning, [], true, SERVER_STATUS.running],
    ["recovers on a later check-in", SERVER_STATUS.incompatible, [], true, SERVER_STATUS.running],
    ["agent comes back", SERVER_STATUS.unreachable, [], true, SERVER_STATUS.running],
    ["clean registration waits", SERVER_STATUS.provisioning, [], false, SERVER_STATUS.provisioning],
    ["cleared before any check-in", SERVER_STATUS.incompatible, [], false, SERVER_STATUS.provisioning],
    ["unreachable stays unreachable", SERVER_STATUS.unreachable, [], false, SERVER_STATUS.unreachable],
  ];
  for (const [name, prev, reasons, checkedIn, want] of cases) {
    it(name, () => {
      expect(nextServerStatus(prev, reasons, checkedIn)).toBe(want);
    });
  }
});

describe("an unknown server type", () => {
  it("is not judged", () => {
    // A type outside the catalog can only be a legacy row; inventing an
    // incompatibility for it would attach a reason no requirement backs.
    expect(checkServerCompatibility("toaster", { arch: "s390x", distro: "fedora-41" })).toEqual([]);
  });
});

// The demo mirror of the control plane's rule: a type change is evidence of
// nothing about the machine. Passing "has this server ever reported a version"
// as "is it alive now" silently marked an unreachable host running.
describe("a type change never invents liveness", () => {
  const recent = new Date(Date.now() - 5_000);
  const stale = new Date(Date.now() - 10 * 60_000);
  const failing: FailedRequirement[] = [
    { id: "gpu", fact: "gpu", expected: "An NVIDIA GPU.", detected: "none", reason: "no GPU" },
  ];

  it("does not revive an unreachable host", () => {
    expect(statusAfterTypeChange(SERVER_STATUS.unreachable, [], stale)).toBe(
      SERVER_STATUS.unreachable
    );
    expect(statusAfterTypeChange(SERVER_STATUS.incompatible, [], stale)).toBe(
      SERVER_STATUS.unreachable
    );
  });

  it("gives a live host back", () => {
    expect(statusAfterTypeChange(SERVER_STATUS.incompatible, [], recent)).toBe(
      SERVER_STATUS.running
    );
  });

  it("leaves a host that never checked in waiting", () => {
    expect(statusAfterTypeChange(SERVER_STATUS.incompatible, [], null)).toBe(
      SERVER_STATUS.provisioning
    );
  });

  it("does not promote a provisioning host", () => {
    expect(statusAfterTypeChange(SERVER_STATUS.provisioning, [], recent)).toBe(
      SERVER_STATUS.provisioning
    );
  });

  it("marks incompatible whatever the liveness", () => {
    expect(statusAfterTypeChange(SERVER_STATUS.running, failing, recent)).toBe(
      SERVER_STATUS.incompatible
    );
    expect(statusAfterTypeChange(SERVER_STATUS.unreachable, failing, stale)).toBe(
      SERVER_STATUS.incompatible
    );
  });
});
