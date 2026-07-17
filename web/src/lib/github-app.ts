// GitHub App install-flow helpers (SIGMA-55). The install link carries a
// `state` string GitHub round-trips to the App's Setup URL, which is how the
// callback knows which project (new connection) or existing connection the
// installation belongs to. Pure and dependency-free so the parse rules are
// unit-testable — a forged/garbled state must parse to null, never to a
// different target.

export type GitAppInstallTarget =
  | { kind: "project"; projectId: string }
  | { kind: "connection"; projectId: string; connectionId: string };

// Internal ids are prefixed base62-ish tokens; anything else is rejected.
const ID = /^[A-Za-z0-9_-]{1,64}$/;

export function encodeInstallState(target: GitAppInstallTarget): string {
  return target.kind === "project"
    ? `proj:${target.projectId}`
    : `conn:${target.projectId}:${target.connectionId}`;
}

export function parseInstallState(
  state: string | undefined | null
): GitAppInstallTarget | null {
  if (!state) return null;
  const parts = state.split(":");
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
