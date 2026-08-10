// What the server detail page is allowed to ask the control plane for
// (SIGMA-328).
//
// The page renders one server's hosted resources. It used to get them by
// asking for *every* resource in the org — `GET /v1/orgs/{org}/resources` with
// no filter, backed by a store query with no LIMIT — and then dropping the ones
// whose serverId did not match, in the render path of a page request. In an org
// with 2,000 resources across 40 servers that is 2,000 rows including each
// one's full `spec` jsonb pulled over HTTP to render ~50, and it gets slower
// every time a resource is created anywhere in the org, including in projects
// this viewer is not allowed to see.
//
// The rule this test keeps: the page asks the CP for this server's resources,
// so the payload is bounded by what the page actually renders. Project-scope
// filtering stays client-side — the CP has no notion of the viewer's visible
// projects — so that is asserted too.

import { beforeEach, describe, expect, it, vi } from "vitest";

const cpListResources = vi.fn();

vi.mock("next/navigation", () => ({
  redirect: (to: string) => {
    throw new Error(`unexpected redirect to ${to}`);
  },
  notFound: () => {
    throw new Error("unexpected notFound");
  },
}));
vi.mock("@/server/active-org", () => ({
  getActiveOrgId: async () => "org_1",
  requireMembership: async () => ({ user: { id: "usr_1" }, role: "Org Admin" }),
  // Only project p_visible is in scope for this viewer.
  visibleProjects: async () => new Set(["p_visible"]),
}));
vi.mock("@/server/queries", () => ({
  getEnvironment: async (id: string) => ({ id, name: "prod" }),
  getProject: async (id: string) => ({ id, name: "Checkout" }),
  getResource: async (id: string) => ({ id, status: "running" }),
  getServer: async () => null,
  getServerHosted: async () => [],
}));
vi.mock("@/server/cp", () => ({
  cpEnabled: () => true,
  cpGetServer: async (_orgId: string, serverId: string) => ({
    id: serverId,
    name: "host-a",
    lastSeenAt: null,
  }),
  cpListResources: (orgId: string, environmentId?: string, serverId?: string) =>
    cpListResources(orgId, environmentId, serverId),
  cpMetricsToPoints: () => [],
  cpServerMetrics: async () => [],
  cpServerToRow: (cp: { id: string; name: string }) => ({ id: cp.id, name: cp.name }),
}));
vi.mock("@/components/dashboard/servers/server-detail-view", () => ({
  ServerDetailView: (props: Record<string, unknown>) => props,
}));

import ServerDetailPage from "./page";

beforeEach(() => {
  cpListResources.mockReset();
});

describe("server detail page (CP mode)", () => {
  it("requests only the server's resources", async () => {
    cpListResources.mockResolvedValue([]);

    await ServerDetailPage({ params: Promise.resolve({ serverId: "srv_a" }) });

    expect(cpListResources).toHaveBeenCalledTimes(1);
    const [orgId, environmentId, serverId] = cpListResources.mock.calls[0];
    expect(orgId).toBe("org_1");
    expect(environmentId).toBeUndefined();
    // The whole point: the server predicate goes to the control plane, not to a
    // .filter() over the org's entire resource table.
    expect(serverId).toBe("srv_a");
  });

  it("still hides resources in projects the viewer cannot see", async () => {
    cpListResources.mockResolvedValue([
      {
        id: "res_1",
        name: "web",
        kind: "app",
        serverId: "srv_a",
        projectId: "p_visible",
        environmentId: "env_1",
        status: {},
      },
      {
        id: "res_2",
        name: "secret-svc",
        kind: "app",
        serverId: "srv_a",
        projectId: "p_hidden",
        environmentId: "env_1",
        status: {},
      },
    ]);

    const el = (await ServerDetailPage({
      params: Promise.resolve({ serverId: "srv_a" }),
    })) as { props: { hosted: Array<{ id: string }> } };

    expect(el.props.hosted.map((h) => h.id)).toEqual(["res_1"]);
  });
});
