import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  DECOMMISSION_TIMEOUT_MS,
  boundResourcesMessage,
  forceReason,
  isDecommissioning,
  removalPlan,
} from "./decommission";
import { MANUAL_UNINSTALL_SCRIPT } from "./uninstall-script";
import { SERVER_STATUS, nextServerStatus, statusAfterTypeChange } from "./server-compat";
import { RAW_STATUS_LABELS, normalizeStatus } from "./status";

// The one that matters most, and the reason uninstall-script.ts exists as a
// separate module: the script the dialog hands out and the script that ships in
// the release must be the same bytes.
//
// It is not a style rule. The person running it has just been told the graceful
// teardown failed, on a host they are trying to get back, and a script that has
// drifted from the agent's real layout — a renamed unit, a moved data dir —
// leaves exactly the residue the whole feature exists to remove, while telling
// them it is gone.
describe("the manual cleanup script", () => {
  const onDisk = readFileSync(
    join(process.cwd(), "..", "agent", "packaging", "uninstall.sh"),
    "utf8"
  );

  it("is byte-identical to agent/packaging/uninstall.sh", () => {
    expect(MANUAL_UNINSTALL_SCRIPT).toBe(onDisk);
  });

  it("removes each thing the agent's own teardown removes", () => {
    // Named against the agent's steps (agent/internal/uninstall): containers
    // and networks, the mesh, the units, the data dir, the binary. A step that
    // is added there and forgotten here is a half-cleaned host.
    for (const fragment of [
      "label=sigmahub.managed=true", // containers/networks/volumes
      "wg-quick down",
      "sigma0.conf",
      "/etc/systemd/system/sigmad.service",
      "/etc/systemd/system/sigmahub-nftables.service",
      "/usr/local/bin/sigmad",
    ]) {
      expect(onDisk).toContain(fragment);
    }
  });

  it("keeps volumes unless asked, like every other layer", () => {
    expect(onDisk).toContain("--purge-volumes");
    expect(onDisk).toContain("keeping named volumes");
  });
});

describe("removalPlan", () => {
  it("says volumes are KEPT by default, and names them as data", () => {
    const kept = removalPlan(false);
    const volumes = kept.find((i) => i.label.toLowerCase().includes("volume"));
    expect(volumes?.label).toContain("kept");
    expect(volumes?.destructive).toBeFalsy();
  });

  it("turns the volume line destructive when the operator opts in", () => {
    const purge = removalPlan(true);
    const volumes = purge.find((i) => i.label.toLowerCase().includes("volume"));
    expect(volumes?.destructive).toBe(true);
    expect(volumes?.detail).toMatch(/permanently|no undo/i);
  });

  it("promises the tunnel and the agent itself go — the thing the old toast lied about", () => {
    const labels = removalPlan(false).map((i) => i.label.toLowerCase());
    expect(labels.some((l) => l.includes("wireguard") || l.includes("tunnel"))).toBe(true);
    expect(labels.some((l) => l.includes("agent"))).toBe(true);
  });

  it("is explicit that the host firewall is left alone", () => {
    const firewall = removalPlan(false).find((i) => i.label.toLowerCase().includes("firewall"));
    expect(firewall).toBeDefined();
    expect(firewall?.destructive).toBeFalsy();
  });
});

describe("forceReason", () => {
  const now = Date.UTC(2026, 0, 1, 12, 0, 0);

  it("is not offered for a healthy server — the graceful path is the product", () => {
    expect(forceReason({ status: SERVER_STATUS.running, now })).toBeNull();
    expect(
      forceReason({
        status: SERVER_STATUS.running,
        lastSeenAt: new Date(now - 20_000),
        now,
      })
    ).toBeNull();
  });

  it("is not offered while a teardown is still inside its window", () => {
    expect(
      forceReason({
        status: SERVER_STATUS.decommissioning,
        decommissioningSince: new Date(now - 60_000),
        now,
      })
    ).toBeNull();
  });

  it("is offered once the teardown passes the control plane's timeout", () => {
    const reason = forceReason({
      status: SERVER_STATUS.decommissioning,
      decommissioningSince: new Date(now - DECOMMISSION_TIMEOUT_MS - 1),
      now,
    });
    expect(reason?.kind).toBe("timedOut");
    expect(reason?.message).toMatch(/cleanup script/i);
  });

  it("is offered for an unreachable server — nothing is listening to be asked", () => {
    expect(forceReason({ status: SERVER_STATUS.unreachable, now })?.kind).toBe("unreachable");
  });

  it("is offered for a server that has quietly stopped heartbeating", () => {
    expect(
      forceReason({
        status: SERVER_STATUS.running,
        lastSeenAt: new Date(now - 10 * 60_000),
        now,
      })?.kind
    ).toBe("unreachable");
  });

  it("ignores an unparseable timestamp rather than offering the destructive path", () => {
    expect(forceReason({ status: SERVER_STATUS.running, lastSeenAt: "not a date", now })).toBeNull();
    expect(
      forceReason({
        status: SERVER_STATUS.decommissioning,
        decommissioningSince: null,
        now,
      })?.kind
    ).toBeUndefined();
  });
});

describe("boundResourcesMessage", () => {
  it("names one blocker", () => {
    expect(boundResourcesMessage(["web"])).toBe(
      "web still runs on this server. Move or delete it, then disconnect."
    );
  });

  it("names several", () => {
    expect(boundResourcesMessage(["api", "web"])).toBe(
      "api and web still run on this server. Move or delete them, then disconnect."
    );
  });

  it("is empty when nothing is in the way", () => {
    expect(boundResourcesMessage([])).toBe("");
  });
});

describe("the decommissioning status", () => {
  it("is recognised", () => {
    expect(isDecommissioning(SERVER_STATUS.decommissioning)).toBe(true);
    for (const other of [
      SERVER_STATUS.running,
      SERVER_STATUS.provisioning,
      SERVER_STATUS.unreachable,
      SERVER_STATUS.incompatible,
    ]) {
      expect(isDecommissioning(other)).toBe(false);
    }
  });

  // The mirror of the control plane's terminal rule (store/compat.go). Demo
  // mode is where the states are learned, so a demo that let a check-in undo a
  // decommission would be teaching the wrong model — and it is the same defect
  // the CP guard prevents: the host most likely to be disconnected is an
  // incompatible one, and it keeps checking in while it tears itself down.
  it("survives a check-in that fails the compatibility gate", () => {
    const refused = [
      { id: "gpu" as const, fact: "gpu", expected: "", detected: "none", reason: "no GPU" },
    ];
    expect(nextServerStatus(SERVER_STATUS.decommissioning, refused, true)).toBe(
      SERVER_STATUS.decommissioning
    );
    expect(nextServerStatus(SERVER_STATUS.decommissioning, [], true)).toBe(
      SERVER_STATUS.decommissioning
    );
    expect(statusAfterTypeChange(SERVER_STATUS.decommissioning, refused, new Date())).toBe(
      SERVER_STATUS.decommissioning
    );
  });

  // Two one-line map entries, in two different files, and the row is wrong in a
  // different way if either is missing: without the alias the badge falls back
  // to a grey "Unknown" pill and every running-server aggregate has to guess;
  // without the label the machine being removed reads "Provisioning".
  it("renders as in-flight but reads as itself", () => {
    expect(normalizeStatus(SERVER_STATUS.decommissioning)).toBe("provisioning");
    expect(RAW_STATUS_LABELS[SERVER_STATUS.decommissioning]).toBe("Decommissioning");
  });

  // It is not a running server. Every aggregate that counts fleet capacity goes
  // through this normalization, and the control plane has already stopped
  // billing it.
  it("is never counted as running", () => {
    expect(normalizeStatus(SERVER_STATUS.decommissioning)).not.toBe("running");
  });
});

// The timeout is a hand copy of the control plane's, and every other CP↔web
// constant in this codebase is either generated or digest-guarded. Drift here
// is silent and user-visible in both directions: too low and the dialog offers
// Force disconnect while the CP is still waiting for the agent; too high and
// the row sits with no force affordance long after the CP gave up.
describe("the decommission timeout matches the control plane", () => {
  it("equals the sweeper's DecommissionTimeout", () => {
    const main = readFileSync(
      join(process.cwd(), "..", "cp", "cmd", "sigmahub-cp", "main.go"),
      "utf8"
    );
    const m = main.match(/DecommissionTimeout:\s*(\d+)\s*\*\s*time\.(Minute|Second)/);
    expect(m, "DecommissionTimeout not found in cp/cmd/sigmahub-cp/main.go").toBeTruthy();
    const ms = Number(m![1]) * (m![2] === "Minute" ? 60_000 : 1_000);
    expect(
      DECOMMISSION_TIMEOUT_MS,
      "the dialog and the sweeper disagree about how long a teardown gets"
    ).toBe(ms);
  });
});
