"use client";

import * as React from "react";

/**
 * Carries the server's answer to "does this deployment deliver email?" down to
 * the auth screens (SIGMA-307).
 *
 * Same shape, and same reason, as AuthProvidersProvider: /forgot is a client
 * component — it owns form state — so it cannot read process.env itself, and
 * the (auth) layout is a server component that can. The context default is
 * FALSE so a screen rendered outside the provider promises nothing: the failure
 * mode of this switch must be an over-cautious message, never a "check your
 * inbox" for mail that was printed to a log.
 */
const MailDeliveryContext = React.createContext(false);

export function MailDeliveryProvider({
  value,
  children,
}: {
  value: boolean;
  children: React.ReactNode;
}) {
  return (
    <MailDeliveryContext.Provider value={value}>{children}</MailDeliveryContext.Provider>
  );
}

/** True when a message the server sends actually reaches a mailbox. */
export function useMailDelivery(): boolean {
  return React.useContext(MailDeliveryContext);
}
