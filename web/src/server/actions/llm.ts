"use server";

// The model picker's reads (SIGMA-213/214).
//
// Both are read-only and any member may run them: choosing a model is part of
// filling in the deploy form, and gating the SEARCH behind Project Admin would
// mean a developer could type a repo id but not look one up — a permission
// model that only makes the mistake more likely.
//
// Neither throws. Next.js redacts thrown server-action messages in production,
// so a throw reaches the picker as the "Server Components render" digest string
// and nothing else — the same reason detectRepo returns its failure (SIGMA-208).
// A failure here is a spinner someone is watching, so it comes back as text with
// a Retry next to it.

import { assertProjectVisible, requireMembership } from "../active-org";
import { cpEnabled, cpResolveModel, cpSearchModels } from "../cp";
import { MOCK_TOKEN_CONFIGURED, findMockModel, searchMockModels } from "@/lib/mock/models";
import type { ModelCard, ModelSearchResult } from "@/lib/wizard/llm";

/** What resolving one repo id answered.
 *
 *  `card: null, error: undefined` is a real answer — the Hub does not know this
 *  id — and it is NOT a reason to stop: the control plane creates the resource
 *  with the reference exactly as typed and skips the fit check. `error` set is
 *  the other thing entirely: nobody could ask. Only the second is retryable. */
export type ModelResolveResult = { card: ModelCard | null; error?: string };

/** Free-text model search, the picker's typeahead. An empty query is legal and
 *  returns the catalogue's own ordering, which is what fills the list before
 *  anyone has typed. */
export async function searchModels(input: {
  orgId: string;
  query: string;
  limit?: number;
  /** The project this endpoint is being created in, when the wizard knows it —
   *  it is what turns `tokenConfigured` into an answer about THIS operator's
   *  weights credential rather than about the control plane's own. Absent from
   *  the standalone /dashboard/resources route, where the project is not chosen
   *  until the target step; see cpSearchModels for what each route is told. */
  projectId?: string;
}): Promise<ModelSearchResult> {
  await requireMembership(input.orgId);
  // The project id arrives from a client, and the answer it buys is drawn from
  // that project's secrets — so it goes through the same read gate every other
  // client-supplied project id does (SIGMA-84). Without it a scoped member
  // could ask whether a project they cannot see holds a Hub token.
  const projectId = input.projectId?.trim() ?? "";
  if (projectId) await assertProjectVisible(input.orgId, projectId);
  if (!cpEnabled()) {
    // Demo mode has no control plane and therefore no Hub and no token. The
    // catalogue is a recording of real cards so the picker, the VRAM filter and
    // the gated refusal are all walkable offline (SIGMA-215).
    return {
      models: searchMockModels(input.query, input.limit),
      tokenConfigured: MOCK_TOKEN_CONFIGURED,
    };
  }
  try {
    return await cpSearchModels(input.orgId, input.query.trim(), input.limit, projectId);
  } catch (err) {
    return {
      models: [],
      // Not "tokenConfigured: true" on a failure: claiming credentials we could
      // not confirm would suppress the gated-model warning, which is the one
      // thing on this step the user cannot discover any other way.
      tokenConfigured: false,
      error: `Couldn't reach the model catalogue: ${
        err instanceof Error ? err.message : "unknown error"
      }. Try again, or type the repo id — it is used exactly as typed.`,
    };
  }
}

/** Resolve one repo id to the same card the search would return, so a model
 *  that arrived by paste is sized and fit-checked exactly like a picked one. */
export async function resolveModel(input: {
  orgId: string;
  id: string;
}): Promise<ModelResolveResult> {
  await requireMembership(input.orgId);
  const id = input.id.trim();
  if (!id) return { card: null };
  if (!cpEnabled()) {
    return { card: findMockModel(id) ?? null };
  }
  try {
    return { card: await cpResolveModel(input.orgId, id) };
  } catch (err) {
    return {
      card: null,
      error: `Couldn't look up ${id}: ${
        err instanceof Error ? err.message : "unknown error"
      }. Try again, or continue — the id is used exactly as typed.`,
    };
  }
}
