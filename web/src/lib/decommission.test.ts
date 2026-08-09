import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  DECOMMISSION_TIMEOUT_MS,
  DEMO_TEARDOWN_STEP_MS,
  boundResourcesMessage,
  demoTeardownPhase,
  demoTeardownSpanMs,
  forceReason,
  isDecommissioning,
  msUntilNextTeardownStep,
  removalPlan,
  mayAckDemoTeardown,
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

// The demo teardown (SIGMA-215). Its job is to end: with no control plane there
// is no agent to ack, so pressing Disconnect used to leave the row in
// `decommissioning` for good unless the operator noticed a simulate button.
describe("the demo teardown", () => {
  const START = Date.UTC(2027, 4, 1, 12, 0, 0);
  const at = (elapsed: number) => ({
    startedAt: new Date(START),
    purgeVolumes: false,
    now: START + elapsed,
  });

  it("starts on the step the agent starts on — the containers", () => {
    const phase = demoTeardownPhase(at(0));
    expect(phase.step).toBe(0);
    expect(phase.label.toLowerCase()).toContain("container");
    expect(phase.done).toBe(false);
  });

  it("walks one step per interval, in the agent's own order", () => {
    const labels = [0, 1, 2].map((i) => demoTeardownPhase(at(i * DEMO_TEARDOWN_STEP_MS)).label);
    expect(labels[0].toLowerCase()).toContain("container");
    expect(labels[1].toLowerCase()).toContain("wireguard");
    expect(labels[2].toLowerCase()).toContain("agent");
  });

  // The one destructive step is only walked when the operator opted in, and it
  // is named while it happens rather than only in the confirmation dialog.
  it("says it is deleting volumes only when that was asked for", () => {
    const kept = demoTeardownPhase(at(DEMO_TEARDOWN_STEP_MS));
    expect(kept.label.toLowerCase()).not.toContain("volume");
    const purged = demoTeardownPhase({
      startedAt: new Date(START),
      purgeVolumes: true,
      now: START + DEMO_TEARDOWN_STEP_MS,
    });
    expect(purged.label.toLowerCase()).toContain("volume");
    expect(purged.total).toBe(kept.total + 1);
  });

  it("finishes, which is the whole reason it exists", () => {
    expect(demoTeardownPhase(at(demoTeardownSpanMs(false))).done).toBe(true);
  });

  // The boundary anything meant to sit outside the demo teardown is derived
  // from — the seeded in-flight fixture above all. It has to land on the last
  // step's end and not a step short: a row placed just past a span that
  // understated itself is still mid-teardown, and the page that loads it acks a
  // server nobody asked about.
  it("spans the sequence it is about to walk, to its last step and no further", () => {
    for (const purgeVolumes of [false, true]) {
      const span = demoTeardownSpanMs(purgeVolumes);
      const phaseAt = (now: number) =>
        demoTeardownPhase({ startedAt: new Date(START), purgeVolumes, now });
      expect(phaseAt(START + span - 1).done, `purgeVolumes=${purgeVolumes}`).toBe(false);
      expect(phaseAt(START + span).done, `purgeVolumes=${purgeVolumes}`).toBe(true);
    }
    // The opted-in volume step is a step like any other, and it lengthens the
    // teardown a watcher has to be present for.
    expect(demoTeardownSpanMs(true)).toBe(demoTeardownSpanMs(false) + DEMO_TEARDOWN_STEP_MS);
  });

  // `done` is this clock reporting that its seconds are spent, and nothing
  // more: it answers identically for a teardown that finished under a watching
  // page and for one seeded a week ago that nobody ever saw. Reading it as "the
  // agent reported in" is what deleted a server on first paint, so the ack is
  // the servers page's decision — made only for a teardown it watched start —
  // and never this function's.
  it("says the same thing about a teardown nobody watched as about one that just finished", () => {
    expect(demoTeardownPhase(at(demoTeardownSpanMs(false))).done).toBe(true);
    expect(demoTeardownPhase(at(7 * 86_400_000)).done).toBe(true);
  });

  // Ten seconds, not ten minutes. The timeout is what the "never answers"
  // simulation reaches, and nobody watching a demo can sit through it.
  it("completes far inside the control plane's patience", () => {
    expect(demoTeardownSpanMs(true)).toBeLessThan(DECOMMISSION_TIMEOUT_MS / 10);
  });

  it("treats a row with no start time as already finished, never as stuck", () => {
    const phase = demoTeardownPhase({ startedAt: null, purgeVolumes: false, now: START });
    expect(phase.done).toBe(true);
  });

  it("asks to be looked at again exactly when the next step lands", () => {
    expect(msUntilNextTeardownStep(at(1_000))).toBe(DEMO_TEARDOWN_STEP_MS - 1_000);
    expect(msUntilNextTeardownStep(at(demoTeardownSpanMs(false)))).toBeNull();
  });
});

// The ack DELETES a server, so this predicate is the thing standing between a
// page load and a machine leaving the fleet. It was a pair of booleans inside a
// render body until adversarial review mutated it five ways — dropping either
// half of the condition, or the guard reading it — and all 543 tests stayed
// green while a fresh demo went back to tombstoning a host nobody had touched.
// Every case below fails if one of those mutations comes back.
describe("who may write a demo teardown's ack", () => {
  const START = 1_700_000_000_000;
  const watching = {
    status: SERVER_STATUS.decommissioning,
    startedAt: new Date(START),
    watchedFromStart: true,
  };

  it("lets a page that watched the teardown start finish it", () => {
    expect(mayAckDemoTeardown({ ...watching, now: START + demoTeardownSpanMs(false) })).toBe(true);
  });

  it("refuses a page that arrived after the fact, however finished the clock says it is", () => {
    // The seeded fixture, and any tab opened a moment late. Writing the ack
    // here claims an agent reported in to a page that never saw one.
    expect(
      mayAckDemoTeardown({ ...watching, watchedFromStart: false, now: START + 60_000 })
    ).toBe(false);
  });

  it("refuses once the control plane's window has closed, even to the page that watched", () => {
    // What is true about that row is that nothing answered. Force disconnect
    // and the cleanup script are the next honest move, not a tombstone written
    // on the agent's behalf — and this is what makes "the agent never answers"
    // reach the state it advertises instead of deleting the server whose force
    // path it exists to demonstrate.
    expect(mayAckDemoTeardown({ ...watching, now: START + DECOMMISSION_TIMEOUT_MS })).toBe(false);
  });

  it("refuses a server that is not being decommissioned at all", () => {
    expect(mayAckDemoTeardown({ ...watching, status: SERVER_STATUS.running })).toBe(false);
  });

  it("needs both conditions, not either one", () => {
    // Pinned as a pair because each mutation that dropped one of them left the
    // other looking sufficient.
    expect(
      mayAckDemoTeardown({ ...watching, watchedFromStart: false, now: START + 1_000 })
    ).toBe(false);
    expect(
      mayAckDemoTeardown({ ...watching, now: START + DECOMMISSION_TIMEOUT_MS + 1 })
    ).toBe(false);
  });
});
