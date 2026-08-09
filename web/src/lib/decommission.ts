// The disconnect dialog's logic (SIGMA-205), kept out of the component so it
// can be tested and so both modes ask it the same questions.
//
// The dialog exists because "Disconnect" used to be a menu item with a toast
// that lied: it claimed "the agent tears down its WireGuard tunnel" while the
// binary, the systemd unit, the tunnel, the containers and the volumes all
// stayed on the machine. Three things follow from fixing that, and all three
// are decisions rather than rendering:
//
//   1. state exactly what will be removed, and what will NOT;
//   2. make destroying the customer's data an explicit, separate opt-in;
//   3. when the graceful path cannot work — the host is unreachable, or the
//      teardown timed out — say so, offer the force path, and hand over the
//      script that finishes the job by hand.

import { SERVER_STATUS } from "./server-compat";

/** How long the control plane waits for the agent's ack before it completes the
 *  decommission without one. Mirrors the sweeper's DecommissionTimeout in
 *  cmd/sigmahub-cp; the dashboard uses it to decide when the graceful path has
 *  had its chance and Force disconnect is the honest next step. */
export const DECOMMISSION_TIMEOUT_MS = 10 * 60 * 1000;

/** How long a server may go unheard-from before the control plane's sweeper
 *  calls it unreachable. Mirrors sweeper.StaleAfter. */
export const HEARTBEAT_STALE_MS = 90 * 1000;

/** One line of "here is what happens to your machine". `destructive` marks the
 *  items that destroy something the customer might want back. */
export type RemovalItem = { label: string; detail: string; destructive?: boolean };

/** What a graceful decommission removes, in the order the agent does it, plus
 *  the one thing it deliberately leaves behind.
 *
 *  The order is not decoration: it is the agent's real sequence
 *  (agent/internal/uninstall), and an operator watching a teardown should be
 *  able to follow along. */
export function removalPlan(purgeVolumes: boolean): RemovalItem[] {
  return [
    {
      label: "Running containers and networks",
      detail: "Every container and Docker network SigmaHub created on this host is stopped and removed.",
      destructive: true,
    },
    purgeVolumes
      ? {
          label: "Named volumes (application data)",
          detail:
            "Database data directories and uploaded files on this host are deleted permanently. There is no undo.",
          destructive: true,
        }
      : {
          label: "Named volumes are kept",
          detail:
            "Database data directories and uploaded files stay on the host. They are yours; disconnecting the machine does not delete them.",
        },
    {
      label: "The WireGuard tunnel",
      detail: "The sigma0 interface is brought down and its config and private key are deleted.",
      destructive: true,
    },
    {
      label: "The agent itself",
      detail: "The systemd unit, the /etc/sigmad config, the agent's data directory and the sigmad binary are removed.",
      destructive: true,
    },
    {
      label: "Your firewall rules stay as they are",
      detail:
        "The live nftables ruleset is left alone — dropping a host's firewall as a side effect of returning the machine would be a security change you did not ask for.",
    },
  ];
}

/** Why the graceful path is not available (or not working), or null when it is
 *  the right button to press. */
export type ForceReason =
  | { kind: "unreachable"; message: string }
  | { kind: "timedOut"; message: string }
  | null;

export type ForceInput = {
  status: string;
  /** When the graceful decommission was asked for; null when none is in flight. */
  decommissioningSince?: Date | string | null;
  /** Last heartbeat, for a server that is not formally `unreachable` yet but
   *  has clearly stopped answering. */
  lastSeenAt?: Date | string | null;
  now?: number;
};

function ms(value: Date | string | null | undefined): number | null {
  if (!value) return null;
  const d = value instanceof Date ? value : new Date(value);
  const t = d.getTime();
  return Number.isNaN(t) ? null : t;
}

/** Whether to offer "Force disconnect", and the sentence explaining why.
 *
 *  Only in the two cases where waiting is pointless. Offering it always would
 *  make it the button everyone presses — it is one click and it "works" — and
 *  every press leaves an agent, a tunnel and a set of containers behind on a
 *  machine the dashboard now says nothing about. That is the defect, not the
 *  fix, so the force path has to be the answer to a question the product asked
 *  first. */
export function forceReason(input: ForceInput): ForceReason {
  const now = input.now ?? Date.now();
  const started = ms(input.decommissioningSince);
  if (input.status === SERVER_STATUS.decommissioning && started !== null) {
    if (now - started >= DECOMMISSION_TIMEOUT_MS) {
      return {
        kind: "timedOut",
        message:
          "The agent has not confirmed the teardown. Force disconnect removes the server here; " +
          "run the cleanup script on the host to finish the job.",
      };
    }
    return null; // still in flight — let it work
  }
  if (input.status === SERVER_STATUS.unreachable) {
    return {
      kind: "unreachable",
      message:
        "This server has stopped answering, so it cannot be asked to uninstall anything. " +
        "Force disconnect removes it here; run the cleanup script on the host to finish the job.",
    };
  }
  const seen = ms(input.lastSeenAt);
  if (seen !== null && now - seen > HEARTBEAT_STALE_MS) {
    return {
      kind: "unreachable",
      message:
        "This server has missed its recent heartbeats, so it may not pick up the uninstall. " +
        "Force disconnect removes it here; run the cleanup script on the host to finish the job.",
    };
  }
  return null;
}

/** Whether a graceful decommission is in flight for this server. */
export function isDecommissioning(status: string): boolean {
  return status === SERVER_STATUS.decommissioning;
}

// ── The demo teardown (SIGMA-215) ───────────────────────────────────────────
//
// With no control plane there is no agent to ask and nothing to ack, so the
// graceful path — the one an operator actually takes — had no way to finish.
// Pressing Disconnect left the row in `decommissioning` for good, and the only
// way out was a simulate button the user had to notice. A demo of a state that
// never ends is the infinite spinner this programme exists to delete.
//
// So the demo's default outcome runs on a clock, and the clock walks the
// agent's REAL uninstall sequence (agent/internal/uninstall), the same order
// removalPlan() lists. What an operator learns from watching it is what their
// own machine will do.

/** How long each teardown step takes in demo mode.
 *
 *  2.5 seconds: the whole sequence is then seven and a half to ten seconds
 *  (demoTeardownSpanMs), long enough to read each line as it happens and short
 *  enough that nobody concludes it has hung. It is deliberately unrelated to
 *  DECOMMISSION_TIMEOUT_MS — that is the control plane's ten-MINUTE patience,
 *  and the whole point of the "never answers" simulation is that nobody can sit
 *  through it.
 *
 *  Two clocks three orders of magnitude apart, in one feature, is how the demo
 *  came to delete a server nobody had touched: a fixture stated its age in
 *  minutes and justified it against the ten-minute one, and the servers page
 *  measured the same row against this one, found it long finished, and wrote
 *  the agent's ack. Nothing that has to fall on one side of either boundary may
 *  be written as a number that happens to fit — derive it from
 *  demoTeardownSpanMs for this clock, from DECOMMISSION_TIMEOUT_MS for the
 *  other, and it cannot be made wrong by editing the other file. */
export const DEMO_TEARDOWN_STEP_MS = 2_500;

export type TeardownPhase = {
  /** Steps completed so far, 0-based, capped at `total`. */
  step: number;
  total: number;
  /** What the agent is doing right now, or what it finished doing. */
  label: string;
  /** True once the agent would have reported back — the caller's cue to write
   *  the ack and tombstone the row. */
  done: boolean;
};

/** The steps, in the order the agent performs them. `purgeVolumes` inserts the
 *  one destructive step the operator had to opt into, so a demo that deletes
 *  data says the words while it happens. */
function teardownSteps(purgeVolumes: boolean): string[] {
  return [
    "Stopping containers and removing networks",
    ...(purgeVolumes ? ["Deleting named volumes — application data"] : []),
    "Bringing down the WireGuard tunnel",
    "Removing the agent, its unit and its config",
  ];
}

/** The whole demo teardown, from the request to the ack, for the sequence the
 *  operator's volume choice produces.
 *
 *  It is ABSOLUTE wall time and there is nothing to pause it: no agent reports,
 *  no job runs between requests, and a page that was not open while these
 *  seconds passed missed the entire teardown. So a row older than this span
 *  says only that the span is over — never that anything happened — and code
 *  that reads "finished" as "the agent confirmed" is inventing a report. That
 *  is the boundary a watcher has to be present for, and the number anything
 *  sitting deliberately outside it must be derived from. */
export function demoTeardownSpanMs(purgeVolumes: boolean): number {
  return teardownSteps(purgeVolumes).length * DEMO_TEARDOWN_STEP_MS;
}

/** Where a demo teardown has got to, from the timestamp the decommission was
 *  requested at. Derived rather than stored for the same reason the demo
 *  cluster's node status is: nothing runs between requests here, so a
 *  simulation that needed something to keep writing would stop the moment the
 *  tab did. */
export function demoTeardownPhase(input: {
  startedAt: Date | string | null | undefined;
  purgeVolumes: boolean;
  now?: number;
}): TeardownPhase {
  const steps = teardownSteps(input.purgeVolumes);
  const started = ms(input.startedAt);
  // No timestamp at all means the row predates the request or was written by
  // hand; treating that as finished is the safe answer, because the alternative
  // is a teardown stuck at step zero with nothing that could ever move it.
  if (started === null) {
    return { step: steps.length, total: steps.length, label: steps[steps.length - 1], done: true };
  }
  const elapsed = (input.now ?? Date.now()) - started;
  const completed = Math.max(0, Math.floor(elapsed / DEMO_TEARDOWN_STEP_MS));
  if (completed >= steps.length) {
    return { step: steps.length, total: steps.length, label: steps[steps.length - 1], done: true };
  }
  return { step: completed, total: steps.length, label: steps[completed], done: false };
}

/** How long until the next teardown step, so a watching client can schedule one
 *  render instead of polling. Null once there is nothing left to wait for. */
export function msUntilNextTeardownStep(input: {
  startedAt: Date | string | null | undefined;
  purgeVolumes: boolean;
  now?: number;
}): number | null {
  const phase = demoTeardownPhase(input);
  if (phase.done) return null;
  const started = ms(input.startedAt);
  if (started === null) return null;
  const elapsed = (input.now ?? Date.now()) - started;
  return DEMO_TEARDOWN_STEP_MS - (elapsed % DEMO_TEARDOWN_STEP_MS);
}

/** How the 409 from either disconnect endpoint is turned into something the
 *  dialog can render. The control plane answers with the blocking resource
 *  NAMES as data; printing its error string instead was the previous behaviour
 *  and it read like a stack trace. */
export function boundResourcesMessage(names: string[]): string {
  if (names.length === 0) return "";
  const list = names.length === 1 ? names[0] : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
  return names.length === 1
    ? `${list} still runs on this server. Move or delete it, then disconnect.`
    : `${list} still run on this server. Move or delete them, then disconnect.`;
}
