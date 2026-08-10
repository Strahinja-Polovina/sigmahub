"use server";

// What the control plane behind this dashboard will actually accept (SIGMA-268).
//
// The wizard is built from the generated catalog, which is every engine this
// codebase knows how to provision. A given control plane can be running with a
// smaller set — CP_DB_ENGINES=postgres is a supported, pre-agreed build — and
// its create call refuses the rest with `database engine "x" is not enabled on
// this control plane`. Nothing published that smaller set, so the wizard offered
// the whole catalog and the operator met the disagreement at submit time, with
// the dialog already closed and every decision in it lost.
//
// Demo mode has no control plane to ask and no allowlist to honour: the demo
// implements every engine the catalog defines, so the catalog IS the answer
// there. The same is true when the control plane cannot be reached — see below,
// where a failure deliberately does not narrow the wizard.

import { requireMembership } from "../active-org";
import { DB_ENGINE_CATALOG, S3_ENGINE_NAMES } from "@/lib/server-catalog.generated";
import { cpCapabilities, cpEnabled, type CpCapabilities } from "../cp";

/** Every engine the generated catalog defines — the honest answer whenever no
 *  control plane has narrowed it. */
function catalogCapabilities(): CpCapabilities {
  return {
    dbEngines: Object.keys(DB_ENGINE_CATALOG),
    s3Engines: [...S3_ENGINE_NAMES],
  };
}

/**
 * The engine sets the wizard filters against.
 *
 * Member-visible, like the deploy targets it is shown beside: a developer who
 * cannot read this cannot fill in the form.
 *
 * A control plane that cannot be reached falls back to the full catalog rather
 * than to an empty set, and the direction matters. Narrowing on a failed read
 * would hide engines a working deployment has enabled — turning one bad request
 * into "this product cannot create databases", which is a much worse lie than
 * the one this endpoint exists to fix. Falling back wide degrades to exactly
 * the old behaviour: the create call is still the authority, and it still
 * refuses with a sentence naming the setting.
 */
export async function getEngineCapabilities(orgId: string): Promise<CpCapabilities> {
  await requireMembership(orgId);
  if (!cpEnabled()) return catalogCapabilities();
  try {
    const caps = await cpCapabilities(orgId);
    // A control plane too old to know this route answers 404, which cpFetch
    // throws on; an empty list from a NEW one would mean "nothing is enabled",
    // which no deployment can be — config.FromEnv refuses to boot with an empty
    // allowlist. So treat empty as unknown rather than as a verdict.
    return {
      dbEngines: caps.dbEngines?.length ? caps.dbEngines : catalogCapabilities().dbEngines,
      s3Engines: caps.s3Engines?.length ? caps.s3Engines : catalogCapabilities().s3Engines,
    };
  } catch {
    return catalogCapabilities();
  }
}
