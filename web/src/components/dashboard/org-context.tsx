"use client";

import * as React from "react";
import { getOrg, getOrgs } from "@/lib/mock";
import type { Org } from "@/lib/mock";

const DEFAULT_ORG_ID = "org_acme";

type OrgContextValue = {
  orgId: string;
  org: Org;
  orgs: Org[];
  setOrgId: (id: string) => void;
};

const OrgContext = React.createContext<OrgContextValue | null>(null);

export function OrgProvider({ children }: { children: React.ReactNode }) {
  const [orgId, setOrgId] = React.useState<string>(DEFAULT_ORG_ID);
  const orgs = React.useMemo(() => getOrgs(), []);
  const org = React.useMemo(() => getOrg(orgId), [orgId]);

  const value = React.useMemo<OrgContextValue>(
    () => ({ orgId, org, orgs, setOrgId }),
    [orgId, org, orgs]
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
