/**
 * Surviving the GitHub App install round trip (SIGMA-208).
 *
 * Connecting GitHub from inside the wizard means leaving the dashboard entirely
 * — github.com, an install screen, a redirect back. Without this the return
 * lands on a fresh page with a closed wizard, and everything the user had
 * chosen (the type, the project, the environment) is gone. They then discover
 * that the reward for connecting GitHub is starting over, which is why the
 * previous design pushed them at a settings page and a pasted token instead.
 *
 * The draft is deliberately SMALL and deliberately validated on the way back
 * in: it is written to sessionStorage, which is the user's own machine, so
 * anything read out of it is untrusted input. A garbled or hand-edited draft
 * parses to null and the wizard simply opens empty, which is the behaviour we
 * are trying to avoid but is never worse than acting on a forged one.
 */

import { RESOURCE_KINDS, type ResourceKind } from "@/lib/server-catalog.generated";

/** sessionStorage key. Session-scoped on purpose: a draft that outlived the tab
 *  would reopen the wizard days later over a fleet that has since changed. */
export const WIZARD_RESUME_KEY = "sigmahub.newResource.draft";

/** The query parameter the GitHub callback returns with. */
export const WIZARD_RESUME_PARAM = "wizard";
export const WIZARD_RESUME_VALUE = "resume";

/**
 * What survives the trip. Ids only — names, server lists and detection results
 * are re-read from the server on the way back, because they may have changed
 * while the user was on github.com and a stale copy would be a target that no
 * longer exists.
 */
export type WizardDraft = {
  kind: ResourceKind;
  name?: string;
  projectId?: string;
  environmentId?: string;
  serverId?: string;
  clusterId?: string;
  /** The repository the user had already picked, if any. */
  repo?: string;
  branch?: string;
};

const MAX_FIELD = 200;

function str(v: unknown): string | undefined {
  if (typeof v !== "string") return undefined;
  const s = v.trim();
  if (!s || s.length > MAX_FIELD) return undefined;
  return s;
}

export function encodeWizardDraft(draft: WizardDraft): string {
  return JSON.stringify(draft);
}

/**
 * Read a draft back. Returns null for anything that is not a draft — including
 * a valid JSON object with an unknown `kind`, because the kind decides which
 * steps exist and an unrecognized one is a wizard with no flow at all.
 */
export function decodeWizardDraft(raw: string | null | undefined): WizardDraft | null {
  if (!raw || raw.length > 4000) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return null;
  const o = parsed as Record<string, unknown>;
  const kind = str(o.kind);
  if (!kind || !(RESOURCE_KINDS as string[]).includes(kind)) return null;
  const draft: WizardDraft = { kind: kind as ResourceKind };
  for (const field of ["name", "projectId", "environmentId", "serverId", "clusterId", "repo", "branch"] as const) {
    const value = str(o[field]);
    if (value) draft[field] = value;
  }
  return draft;
}

/**
 * Where the GitHub callback should land so the wizard can reopen itself.
 *
 * A path is BUILT here from an id rather than round-tripped through GitHub's
 * `state`: state is attacker-controllable on the way back, and a "return to
 * this URL" parameter that comes back from a third party is an open redirect
 * with extra steps.
 */
export function wizardResumePath(projectId?: string | null): string {
  const query = `?${WIZARD_RESUME_PARAM}=${WIZARD_RESUME_VALUE}`;
  if (projectId && /^[A-Za-z0-9_-]{1,64}$/.test(projectId)) {
    return `/dashboard/projects/${projectId}${query}`;
  }
  return `/dashboard/resources${query}`;
}

/** Whether a page's query says the wizard should reopen. */
export function shouldResume(params: URLSearchParams | null | undefined): boolean {
  return params?.get(WIZARD_RESUME_PARAM) === WIZARD_RESUME_VALUE;
}
