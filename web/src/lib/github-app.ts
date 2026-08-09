// GitHub App install-flow helpers (SIGMA-55). The install link carries a
// `state` string GitHub round-trips to the App's Setup URL, which is how the
// callback knows which project (new connection) or existing connection the
// installation belongs to. Pure and dependency-free so the parse rules are
// unit-testable — a forged/garbled state must parse to null, never to a
// different target.

export type GitAppInstallTarget =
  // Org-level: the App is connected once for the whole organization and repos
  // are picked from it afterwards. This is the normal path.
  | { kind: "org" }
  // Org-level too, but initiated from inside the New Resource wizard: the
  // callback claims the installation exactly as "org" does and then returns to
  // the page the wizard was opened from, so the flow the user was halfway
  // through can pick itself back up (SIGMA-208). Only the PROJECT id travels —
  // the rest of the draft is on the user's own machine, and a return path that
  // came back from github.com would be an open redirect.
  | { kind: "wizard"; projectId?: string }
  | { kind: "project"; projectId: string }
  | { kind: "connection"; projectId: string; connectionId: string };

// Internal ids are prefixed base62-ish tokens; anything else is rejected.
const ID = /^[A-Za-z0-9_-]{1,64}$/;

export function encodeInstallState(target: GitAppInstallTarget): string {
  switch (target.kind) {
    case "org":
      return "org";
    case "wizard":
      return target.projectId ? `wiz:${target.projectId}` : "wiz";
    case "project":
      return `proj:${target.projectId}`;
    case "connection":
      return `conn:${target.projectId}:${target.connectionId}`;
  }
}

export function parseInstallState(
  state: string | undefined | null
): GitAppInstallTarget | null {
  if (!state) return null;
  if (state === "org") return { kind: "org" };
  if (state === "wiz") return { kind: "wizard" };
  const parts = state.split(":");
  if (parts[0] === "wiz" && parts.length === 2 && ID.test(parts[1])) {
    return { kind: "wizard", projectId: parts[1] };
  }
  if (parts[0] === "proj" && parts.length === 2 && ID.test(parts[1])) {
    return { kind: "project", projectId: parts[1] };
  }
  if (
    parts[0] === "conn" &&
    parts.length === 3 &&
    ID.test(parts[1]) &&
    ID.test(parts[2])
  ) {
    return { kind: "connection", projectId: parts[1], connectionId: parts[2] };
  }
  return null;
}

/** GitHub installation ids are numeric; the callback validates before use. */
export function isInstallationId(v: string | undefined | null): v is string {
  return typeof v === "string" && /^\d{1,20}$/.test(v);
}

export function githubInstallUrl(slug: string, target: GitAppInstallTarget): string {
  const state = encodeURIComponent(encodeInstallState(target));
  return `https://github.com/apps/${encodeURIComponent(slug)}/installations/new?state=${state}`;
}
