// The compatibility gate, for demo mode (SIGMA-203).
//
// The CONTROL PLANE owns this decision. In CP mode nothing here runs: the
// server row arrives with its status and its reasons already decided by
// cp/internal/store/compat.go, and the dashboard renders those sentences
// verbatim rather than re-deriving them — a UI that re-assembled the
// explanation would drift from the API's and contradict it in exactly the
// moment the operator is already confused.
//
// This exists because demo mode has no control plane and no real hardware, and
// "you connected this as a GPU server but there is no GPU" is a state a
// prospective user has to be able to SEE without owning the wrong machine.
//
// The two implementations are kept in step by data, not by discipline:
// cp/internal/store/testdata/compat_cases.json is a table of hosts and the
// exact verdicts they must produce, and it is asserted by BOTH suites — the Go
// one in cp/internal/store/compat_fixture_test.go and the TypeScript one in
// server-compat.test.ts. Change a sentence on either side and one of them goes
// red until the other agrees.

import {
  SERVER_CATALOG,
  SUPPORTED_DISTROS,
  isServerType,
  type RequirementId,
  type ServerType,
} from "./server-catalog.generated";

/** The agent's host description (SIGMA-201), as the control plane stores it.
 *
 *  Every field is optional because an agent older than the fact, or one whose
 *  probe could not answer, simply omits it — and an omitted fact is UNKNOWN,
 *  never empty. The gate below turns on that distinction: `gpu: undefined` is
 *  "nobody looked", `gpu: {count: 0}` is "looked, found nothing". */
export type HostFacts = {
  hostname?: string;
  os?: string;
  arch?: string;
  kernel?: string;
  numCpu?: number;
  memTotalMb?: number;
  /** Catalog distro id ("ubuntu-24.04") read from /etc/os-release — the
   *  machine's own answer, not one picked in a wizard. */
  distro?: string;
  /** os-release PRETTY_NAME, for display. */
  distroName?: string;
  /** BYTES, not a percentage: the catalog states disk floors in bytes and a
   *  percentage cannot be compared against one. */
  diskTotalBytes?: number;
  diskFreeBytes?: number;
  diskPath?: string;
  dockerAvailable?: boolean;
  dockerVersion?: string;
  gpu?: {
    vendor?: string;
    model?: string;
    count?: number;
    vramBytesPerGpu?: number;
    vramBytesTotal?: number;
    driverVersion?: string;
    cards?: { index: number; model: string; vramBytes: number }[];
  } | null;
};

/** One requirement a host violates. Mirrors store.FailedRequirement field for
 *  field, because CP mode delivers these straight out of the API. */
export type FailedRequirement = {
  id: RequirementId;
  fact: string;
  expected: string;
  detected: string;
  /** The whole sentence, rendered verbatim by the UI. */
  reason: string;
};

const DISTRO_LABELS = new Map(SUPPORTED_DISTROS.map((d) => [d.id, d.label]));

/** "a, b or c" — the form the catalog's own sentences read in. */
function joinOr(items: string[]): string {
  if (items.length === 0) return "";
  if (items.length === 1) return items[0];
  return `${items.slice(0, -1).join(", ")} or ${items[items.length - 1]}`;
}

/** A catalog FLOOR, which is always a round number. */
function formatDiskFloor(bytes: number): string {
  const gb = 1_000_000_000;
  if (bytes >= 1000 * gb) {
    // Matches Go's %g: 1 TB, not 1.0 TB.
    return `${bytes / (1000 * gb)} TB`;
  }
  return `${Math.trunc(bytes / gb)} GB`;
}

/** A REPORTED size, which is not: a real 2 TB disk is 1968526655488 bytes, and
 *  "1.968526655488 TB" in a rejection message reads like a bug. */
function formatDiskReported(bytes: number): string {
  const gb = 1_000_000_000;
  if (bytes >= 1000 * gb) {
    return `${(bytes / (1000 * gb)).toFixed(1)} TB`;
  }
  return `${Math.trunc(bytes / gb)} GB`;
}

/** Evaluate a server type's catalog requirements against reported facts.
 *
 *  Returns one entry per violated requirement — empty when the host is
 *  compatible OR when nothing could be evaluated. An absent fact NEVER fails:
 *  see the module comment, and store/compat.go for why that rule is the
 *  difference between a gate and a fleet-wide outage. */
export function checkServerCompatibility(
  serverType: string,
  facts: HostFacts | null | undefined
): FailedRequirement[] {
  if (!isServerType(serverType)) return [];
  const spec = SERVER_CATALOG[serverType as ServerType];
  const f = facts ?? {};
  const req = spec.requires;
  const expected = new Map(req.checks.map((c) => [c.id, c]));
  const out: FailedRequirement[] = [];

  const fail = (id: RequirementId, detected: string, because: string) => {
    const check = expected.get(id);
    out.push({
      id,
      fact: check?.fact ?? id,
      expected: check?.text ?? "",
      detected,
      reason: `You connected this as a ${spec.label} server, but ${because}.`,
    });
  };

  if (req.distros.length > 0 && f.distro && !req.distros.includes(f.distro)) {
    const detected = f.distroName || f.distro;
    const wanted = joinOr(req.distros.map((id) => DISTRO_LABELS.get(id) ?? id));
    fail("distro", detected, `it runs ${detected} — that type needs ${wanted}`);
  }

  if (req.arches.length > 0 && f.arch && !req.arches.includes(f.arch)) {
    fail("arch", f.arch, `its CPU architecture is ${f.arch} — that type needs ${joinOr(req.arches)}`);
  }

  if (req.minDiskBytes > 0 && f.diskTotalBytes && f.diskTotalBytes < req.minDiskBytes) {
    const got = formatDiskReported(f.diskTotalBytes);
    fail("disk", got, `it has ${got} of disk — that type needs at least ${formatDiskFloor(req.minDiskBytes)}`);
  }

  // null/undefined gpu means the agent never looked, which is unknown. An
  // inventory that is present and empty is the host saying "I looked, there is
  // nothing here" — a real reading the gate must act on.
  if (req.gpu && f.gpu) {
    const vendor = req.gpu.vendor.toUpperCase();
    const count = f.gpu.count ?? 0;
    const sameVendor = (f.gpu.vendor ?? "").toLowerCase() === req.gpu.vendor.toLowerCase();
    if (count === 0 || !sameVendor) {
      fail("gpu", count > 0 ? `${count} × ${f.gpu.vendor}` : "none", `no ${vendor} GPU was detected`);
    } else if (req.gpu.driver && !f.gpu.driverVersion) {
      // A card that enumerates over PCI with no working driver fails at the
      // first container start — after the host is enrolled and billed.
      fail("gpu", `${count} × ${f.gpu.vendor}, no driver`, `its ${vendor} GPU has no usable driver`);
    }
  }

  return out;
}

/** Server status vocabulary, including the one SIGMA-203 adds. `incompatible`
 *  is deliberately not a flavour of provisioning: the host is installed and
 *  heartbeating, and only changing its type or disconnecting it will move it. */
/** `decommissioning` (SIGMA-204) is the fifth, and the only TERMINAL one: every
 *  other status is re-derived from what the host reports, this one is a
 *  decision the operator made about the machine's future that no fact can
 *  revise. */
export const SERVER_STATUS = {
  provisioning: "provisioning",
  running: "running",
  unreachable: "unreachable",
  incompatible: "incompatible",
  decommissioning: "decommissioning",
} as const;

/** Whether a server needs the operator to choose between the two exits. */
export function isIncompatible(status: string): boolean {
  return status === SERVER_STATUS.incompatible;
}

/** The status a check-in leaves a server in — the same decision
 *  store.compatibilityStatus makes in the control plane, and deliberately the
 *  same shape so the demo path cannot quietly diverge from it.
 *
 *  `agentCheckedIn` is whether the agent has been heard from: true for a
 *  check-in by definition, and true on the type-change path when the server has
 *  already reported. It is what makes recovery work — a host refused as a GPU
 *  server and then given a driver comes back on its own — without claiming
 *  liveness for a server that has never spoken to us. */
export function nextServerStatus(
  prev: string,
  reasons: FailedRequirement[],
  agentCheckedIn: boolean
): string {
  // A decommissioning server outranks the gate — the same terminal rule as
  // store.compatibilityStatus, and load-bearing for the same reason: one of the
  // two documented exits from `incompatible` IS disconnecting, so the host most
  // likely to be torn down is one whose facts fail its type, and it keeps
  // checking in throughout the teardown. Letting the gate write `incompatible`
  // back over `decommissioning` loses the in-flight state the whole flow hangs
  // off.
  if (prev === SERVER_STATUS.decommissioning) return prev;
  if (reasons.length > 0) return SERVER_STATUS.incompatible;
  if (
    agentCheckedIn &&
    (prev === SERVER_STATUS.provisioning ||
      prev === SERVER_STATUS.unreachable ||
      prev === SERVER_STATUS.incompatible)
  ) {
    return SERVER_STATUS.running;
  }
  if (!agentCheckedIn && prev === SERVER_STATUS.incompatible) {
    return SERVER_STATUS.provisioning;
  }
  return prev;
}

/**
 * The status a server takes when its TYPE is re-filed — the demo mirror of the
 * control plane's statusAfterTypeChange.
 *
 * Separate from nextServerStatus for the same reason it is separate in Go: a
 * heartbeat proves the host is alive, a type change proves nothing at all. The
 * demo path passed "has this server ever reported a version" as "is it alive
 * now", so re-filing the type of an unreachable host silently marked it running
 * — the same defect as the control plane's, and the demo is where an operator
 * first learns what the states mean.
 */
export function statusAfterTypeChange(
  prev: string,
  reasons: FailedRequirement[],
  lastSeenAt: Date | string | null | undefined,
  staleAfterMs = 90_000
): string {
  // Terminal, as in nextServerStatus: a machine on its way out is not re-graded.
  if (prev === SERVER_STATUS.decommissioning) return prev;
  if (reasons.length > 0) return SERVER_STATUS.incompatible;
  // Nothing to clear, so nothing to decide: a re-file neither promotes a
  // provisioning host nor revives an unreachable one.
  if (prev !== SERVER_STATUS.incompatible) return prev;
  if (!lastSeenAt) return SERVER_STATUS.provisioning;
  const seen = lastSeenAt instanceof Date ? lastSeenAt : new Date(lastSeenAt);
  if (Number.isNaN(seen.getTime())) return SERVER_STATUS.provisioning;
  return Date.now() - seen.getTime() > staleAfterMs
    ? SERVER_STATUS.unreachable
    : SERVER_STATUS.running;
}
