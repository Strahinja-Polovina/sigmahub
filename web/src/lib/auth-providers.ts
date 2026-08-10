// Which third-party sign-in methods this deployment actually has (SIGMA-246).
//
// The auth screens used to offer Google, GitHub and passkey buttons on every
// deployment, because the buttons were decoration: nothing behind them was
// configured, so each one toasted "Single sign-on is not wired up in this
// prototype". That is the first sentence the product says to a design partner,
// on the screen before they have an account, and the GitHub button is the one a
// developer reaches for first on a tool that deploys from GitHub.
//
// One source of truth, read in two places that must never disagree:
//   - lib/auth.ts, which hands these credentials to better-auth, and
//   - the (auth) layout, which decides whether the buttons exist at all.
// A provider counts as configured only when BOTH halves of its OAuth credential
// are present, because half a credential is a sign-in that fails at the
// redirect rather than one that never appears.
//
// Deliberately not a NEXT_PUBLIC_ variable: those are inlined at build time, and
// SigmaHub is self-hosted from a prebuilt image — an operator wiring Google on
// Tuesday must not have to rebuild the web bundle to see the button.

/** Providers that can be configured. Passkey is absent on purpose: better-auth
 *  is set up here without the passkey plugin, so there is no credential an
 *  operator could set and no flow the button could start. Add the plugin and
 *  the flag together, or not at all. */
export type AuthProviderFlags = {
  google: boolean;
  github: boolean;
};

/** The safe default — used as the React context default so a tree that somehow
 *  renders outside the provider offers nothing rather than everything. */
export const NO_AUTH_PROVIDERS: AuthProviderFlags = { google: false, github: false };

export function configuredAuthProviders(
  env: Record<string, string | undefined> = process.env
): AuthProviderFlags {
  const both = (id?: string, secret?: string) => Boolean(id?.trim() && secret?.trim());
  return {
    google: both(env.AUTH_GOOGLE_CLIENT_ID, env.AUTH_GOOGLE_CLIENT_SECRET),
    github: both(env.AUTH_GITHUB_CLIENT_ID, env.AUTH_GITHUB_CLIENT_SECRET),
  };
}

/** Whether to show the "or sign up with" block at all — the divider is part of
 *  the offer, so it disappears with the last provider. */
export function anyAuthProvider(flags: AuthProviderFlags): boolean {
  return flags.google || flags.github;
}
