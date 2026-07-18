// Where to send a user after a successful sign-in/sign-up. An invite token in
// the URL (?invite=<token>) routes them straight to the accept page so the
// invite completes in one hop; otherwise the dashboard. Read from
// window.location, so only ever called from client event handlers (SSR-safe:
// returns the dashboard when window is absent).
export function destAfterAuth(): string {
  if (typeof window === "undefined") return "/dashboard";
  const invite = new URLSearchParams(window.location.search).get("invite");
  return invite ? `/invite/${encodeURIComponent(invite)}` : "/dashboard";
}
