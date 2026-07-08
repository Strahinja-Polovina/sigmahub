"use client";

import * as React from "react";

export type OrgSummary = {
  id: string;
  name: string;
  slug: string;
  plan: string;
  serverCount: number;
};
export type CurrentUser = { id: string; name: string; email: string };

type OrgContextValue = {
  orgId: string;
  org: OrgSummary;
  orgs: OrgSummary[];
  user: CurrentUser;
  setOrgId: (id: string) => void;
};

const OrgContext = React.createContext<OrgContextValue | null>(null);

/** Client mirror of the server-resolved active org. Seeded entirely from props
 *  the layout fetches from the DB — no mock data. `setOrgId` gives instant
 *  switch feedback; the org-switcher then persists the `sh_org` cookie so
 *  server components follow on the next refresh (the two never drift). */
export function OrgProvider({
  children,
  orgs,
  activeOrgId,
  user,
}: {
  children: React.ReactNode;
  orgs: OrgSummary[];
  activeOrgId: string;
  user: CurrentUser;
}) {
  const [orgId, setOrgId] = React.useState(activeOrgId);
  // Re-sync when the server changes the active org (e.g. after a switch).
  React.useEffect(() => setOrgId(activeOrgId), [activeOrgId]);

  const org = React.useMemo(
    () => orgs.find((o) => o.id === orgId) ?? orgs[0],
    [orgs, orgId]
  );

  const value = React.useMemo<OrgContextValue>(
    () => ({ orgId, org, orgs, user, setOrgId }),
    [orgId, org, orgs, user]
  );

  return <OrgContext.Provider value={value}>{children}</OrgContext.Provider>;
}

export function useActiveOrg() {
  const ctx = React.useContext(OrgContext);
  if (!ctx) {
    throw new Error("useActiveOrg must be used within an OrgProvider.");
  }
  return ctx;
}
