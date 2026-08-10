import { describe, it, expect, vi } from "vitest";
import * as React from "react";
import { renderToStaticMarkup } from "react-dom/server";

// The panel is a client component: it reaches for the toaster, the domain
// server actions and (through the DNS dialog) the DNS action at import time.
// None of them can run here and none of them is what these tests are about.
vi.mock("sonner", () => ({
  toast: Object.assign(() => {}, { success: () => {}, error: () => {} }),
}));
vi.mock("@/server/actions/domains", () => ({
  attachDomain: vi.fn(),
  detachDomain: vi.fn(),
}));
vi.mock("@/server/actions/dns", () => ({ getDomainDNS: vi.fn() }));

import { ResourceDomainsPanel } from "./resource-domains-panel";

function renderPanel(overrides: Record<string, unknown> = {}) {
  return renderToStaticMarkup(
    React.createElement(ResourceDomainsPanel, {
      orgId: "org-1",
      resourceId: "res-1",
      domains: [],
      canManage: true,
      ...overrides,
    } as never)
  );
}

describe("ResourceDomainsPanel edge-role precondition", () => {
  // SIGMA-316: a certificate is issued by the proxy the reconciler renders onto
  // proxy-role servers and nowhere else. Attach a domain to an app on a server
  // without that role and the panel used to promise "sigmahub will issue a
  // certificate once the domain's DNS points here" — a promise no component in
  // the system can keep. The operator then edits their DNS, waits, and the
  // domain stays pending forever with nothing naming the cause.
  it("attaching to a non-proxy server warns before the form", () => {
    const html = renderPanel({
      server: { id: "srv-1", name: "web-03", proxyRole: false },
    });

    expect(html).toContain("web-03");
    expect(html.toLowerCase()).toMatch(/edge server/);
    // The warning is only useful if it leads somewhere: the switch that fixes
    // this lives on the server's own page, which the operator has no other
    // reason to visit.
    expect(html).toContain("/dashboard/servers/srv-1");
  });

  it("says nothing extra when the server carries the edge role", () => {
    const html = renderPanel({
      server: { id: "srv-1", name: "edge-01", proxyRole: true },
    });

    expect(html).not.toMatch(/not an edge server/i);
  });

  it("stays quiet when the role is unknown", () => {
    // Demo mode and a cluster workload both reach this panel without a server
    // whose role we know. Warning there would cry wolf on every domain.
    expect(renderPanel()).not.toMatch(/not an edge server/i);
    expect(renderPanel({ server: { id: "srv-1", name: "web-03" } })).not.toMatch(
      /not an edge server/i
    );
  });
});
