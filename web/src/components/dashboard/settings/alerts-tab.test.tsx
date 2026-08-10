// The alerts rules editor renders one chip per event the control plane serves,
// labelled from EVENT_LABELS. Both sides held the same vocabulary with nothing
// forcing them to agree, and they disagreed: payment_failed had no label, so on
// a billing-enabled deployment the one alert a paying customer most wants to
// enable rendered as raw "payment_failed" beside sentence-case neighbours,
// looking like a leaked internal identifier — with every suite green
// (SIGMA-274).
//
// This test iterates the generated union (rendered from store.AlertEvents), so
// an event added on the Go side fails here as well as at `tsc --noEmit`, which
// the total Record now catches at the point of the omission.

import { describe, expect, it } from "vitest";

import { ALERT_EVENTS } from "@/lib/server-catalog.generated";
import { EVENT_LABELS, alertEventLabel } from "./alerts-tab";

describe("every event the CP publishes has a label", () => {
  it("labels the whole vocabulary, in words rather than event keys", () => {
    expect(ALERT_EVENTS.length).toBeGreaterThan(0);
    for (const ev of ALERT_EVENTS) {
      const label = EVENT_LABELS[ev];
      expect(label, `${ev} has no label — the chip would render its raw key`).toBeDefined();
      expect(label).not.toBe(ev);
      // A label is prose: no snake_case identifier leaking through.
      expect(label).not.toMatch(/_/);
    }
  });

  it("renders an unknown event as itself rather than as undefined", () => {
    // A dashboard talking to a NEWER control plane sees events this build has
    // never heard of. Showing the key is honest; showing "undefined" is not.
    expect(alertEventLabel("some_future_event")).toBe("some_future_event");
    expect(alertEventLabel("payment_failed")).toBe(EVENT_LABELS.payment_failed);
  });
});
