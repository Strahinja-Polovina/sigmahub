"use client";

import { createAuthClient } from "better-auth/react";
import { twoFactorClient } from "better-auth/client/plugins";

// Talks to /api/auth/* on the same origin. Prod swaps the backend to GCP
// Identity Platform behind the same client surface.
export const authClient = createAuthClient({
  plugins: [twoFactorClient()],
});

export const { signIn, signUp, signOut, useSession } = authClient;
