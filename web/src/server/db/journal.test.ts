// The guard for a defect that produced no output at all.
//
// Drizzle applies a migration only when its journal `when` is greater than the
// newest timestamp already recorded in the database. One entry (0005) was
// written by hand with a `when` three days in the future, which made it a
// ceiling: every migration authored afterwards carried an honest, smaller
// timestamp and was skipped in silence. Two migrations' worth of columns never
// reached `servers`, and the symptom surfaced hundreds of lines away as
// "Control plane unreachable".
//
// Nothing in the repository could have caught that. The SQL was valid, the
// files were numbered correctly, `idx` was in order, both suites passed, and
// the migrator exited 0. The only observable difference between a journal that
// works and one that does not is whether the timestamps ascend — so that is
// what is asserted here, on every run, for every entry.

import { describe, expect, it } from "vitest";
import journal from "../../../drizzle/meta/_journal.json";
import { CORRECTED_WHEN, POISONED_WHEN } from "./migrate-repair";

type Entry = { idx: number; tag: string; when: number };
const entries = journal.entries as Entry[];

describe("the drizzle migration journal", () => {
  it("has entries", () => {
    // A passing suite over an empty array would be the same kind of silence.
    expect(entries.length).toBeGreaterThan(0);
  });

  it("timestamps ascend, because the migrator applies by timestamp and not by file order", () => {
    const outOfOrder = entries
      .map((e, i) => ({ e, prev: entries[i - 1] }))
      .filter(({ e, prev }) => prev && e.when <= prev.when)
      .map(
        ({ e, prev }) =>
          `${e.tag} (when=${e.when}) does not come after ${prev.tag} (when=${prev.when}) — ` +
          `drizzle will SKIP it and every migration after it, silently`
      );
    expect(outOfOrder).toEqual([]);
  });

  it("numbers entries in the same order as it timestamps them", () => {
    // idx is what a human reads and `when` is what the migrator obeys. They
    // agreed everywhere except in the one place that broke production.
    expect(entries.map((e) => e.idx)).toEqual(
      [...entries].sort((a, b) => a.when - b.when).map((e) => e.idx)
    );
  });

  it("no longer carries the poisoned timestamp anywhere", () => {
    expect(entries.map((e) => e.when)).not.toContain(POISONED_WHEN);
  });

  it("keeps the repair's corrected value in step with the journal entry it rewrites", () => {
    // repairMigrationLedger writes CORRECTED_WHEN into the database's ledger.
    // If that constant and the journal ever disagreed, a repaired database
    // would sit at a high-water mark no migration matches — the same failure
    // again, in a fix that was supposed to end it.
    const repaired = entries.find((e) => e.when === CORRECTED_WHEN);
    expect(repaired?.tag).toBe("0005_mongodb_kind_vocabulary");
  });
});
