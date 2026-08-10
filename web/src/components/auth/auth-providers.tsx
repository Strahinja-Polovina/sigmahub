"use client";

import * as React from "react";

import { NO_AUTH_PROVIDERS, type AuthProviderFlags } from "@/lib/auth-providers";

/**
 * Carries the server's answer to "which sign-in providers are configured?" down
 * to the auth screens (SIGMA-246).
 *
 * /login and /signup are client components — they own form state — so they have
 * no way to read process.env themselves. The (auth) layout is a server
 * component, it reads the environment on every request, and this is how the
 * answer reaches its children. The context default is NO_AUTH_PROVIDERS so a
 * screen rendered outside the provider offers nothing at all: the failure mode
 * of this switch must be a missing button, never a dead one.
 */
const AuthProvidersContext = React.createContext<AuthProviderFlags>(NO_AUTH_PROVIDERS);

export function AuthProvidersProvider({
  value,
  children,
}: {
  value: AuthProviderFlags;
  children: React.ReactNode;
}) {
  // The object identity is stable per request on the server; memoise so a
  // client re-render of the layout does not re-render every consumer.
  const memo = React.useMemo(
    () => ({ google: value.google, github: value.github }),
    [value.google, value.github]
  );
  return (
    <AuthProvidersContext.Provider value={memo}>{children}</AuthProvidersContext.Provider>
  );
}

export function useAuthProviders(): AuthProviderFlags {
  return React.useContext(AuthProvidersContext);
}
