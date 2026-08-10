// The demo teardown's endings, and the one thing that must not be one of them.
//
// simulateDecommission is where a demo server actually stops existing: two of
// its four events delete the row. Nothing covered it, and a reviewer proved the
// cost by mutating the guard — the suite stayed green while a fresh demo went
// back to deleting a host on first paint, and while the button whose whole job
// is to reach the force path deleted the server instead of ageing it.
//
// The page-side half of that decision lives in mayAckDemoTeardown and is tested
// in lib/decommission.test.ts. This file is the second line: whatever a caller
// asks for, the action itself refuses to report on a teardown nobody started.
//
// Runs against a real migrated PGlite database (see @/server/testing/demo-db),
// because what broke was never a helper — it was the wiring.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { eq } from "drizzle-orm";

import { DECOMMISSION_TIMEOUT_MS } from "@/lib/decommission";
import { SERVER_STATUS } from "@/lib/server-compat";
import * as s from "@/server/db/schema";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
vi.mock("@/server/active-org", () => {
  const actor = { user: { id: "usr_you", name: "you" }, role: "Org Admin" };
  return {
    requireMembership: async () => actor,
    requireProjectAdmin: async () => actor,
    getActiveOrgId: async () => FIXTURE.orgId,
  };
});
// cpEnabled() false is the branch under test. Every other export throws: in
// demo mode none of them may be reached, and a test that let one through
// silently would be covering the control plane's path by accident.
vi.mock("@/server/cp", () => {
  const forbidden = async () => {
    throw new Error("the CP client must not be called in demo mode");
  };
  return {
    cpEnabled: () => false,
    cpIssueBootstrapToken: forbidden,
    cpProvisionServer: forbidden,
    cpSetHardening: forbidden,
    cpSetProxyRole: forbidden,
    cpPublicUrl: () => "",
    cpDecommissionServer: forbidden,
    cpDeleteServer: forbidden,
    cpGetServer: forbidden,
    boundResourcesOf: () => [],
    cpReissueBootstrapToken: forbidden,
    cpRenameServer: forbidden,
    cpSetServerType: forbidden,
    cpUpdateServerAgent: forbidden,
  };
});

import { decommissionServer, provisionServer, simulateDecommission } from "./servers";
import { db } from "@/server/db";

const database = db as unknown as DemoDb;

/** Put a fixture host into the state a Disconnect leaves behind. */
async function beginTeardown(serverId: string, msAgo: number): Promise<void> {
  await database
    .update(s.servers)
    .set({
      status: SERVER_STATUS.decommissioning,
      decommissionStartedAt: new Date(Date.now() - msAgo),
      decommissionPurgeVolumes: false,
    })
    .where(eq(s.servers.id, serverId));
}

async function serverRow(serverId: string) {
  const [row] = await database.select().from(s.servers).where(eq(s.servers.id, serverId));
  return row;
}

beforeEach(async () => {
  await database.delete(s.clusterNodes);
  await database.delete(s.clusters);
  await database.delete(s.resources);
  await database.delete(s.envServers);
  await database.delete(s.environments);
  await database.delete(s.projects);
  await database.delete(s.servers);
  await database.delete(s.orgs);
  await seedDemoFixture(database);
});

describe("reporting on a demo teardown", () => {
  it("removes the server when the agent acks a teardown that was actually requested", () => {
    return (async () => {
      await beginTeardown(FIXTURE.dbHostId, 1_000);
      await simulateDecommission({ serverId: FIXTURE.dbHostId, event: "ack" });
      expect(await serverRow(FIXTURE.dbHostId)).toBeUndefined();
    })();
  });

  // The defect, stated as a test. A page that arrives after a teardown's clock
  // has run out used to fire this at a row it had merely loaded, and the fleet
  // shrank by one for a visitor who touched nothing.
  it("refuses to report on a server nobody is decommissioning, and says how to start one", async () => {
    await expect(
      simulateDecommission({ serverId: FIXTURE.dbHostId, event: "ack" })
    ).rejects.toThrow(/not being decommissioned/i);
    expect(await serverRow(FIXTURE.dbHostId)).toBeDefined();
  });

  it("refuses a failure report on a running server too, not just an ack", async () => {
    // "failed" deletes the row as well — it is the same hazard wearing the
    // other label, and a guard that only covered the happy ending would have
    // left half the door open.
    await expect(
      simulateDecommission({ serverId: FIXTURE.storageHostId, event: "failed" })
    ).rejects.toThrow(/not being decommissioned/i);
    expect(await serverRow(FIXTURE.storageHostId)).toBeDefined();
  });

  it("ages a teardown past the control plane's window instead of ending it", async () => {
    // The route SIGMA-205 exists for: timeout -> Force disconnect -> the manual
    // cleanup script. This button used to delete the very server whose force
    // path it demonstrates, so what is pinned here is that the row SURVIVES and
    // stays decommissioning, with a clock old enough for the dialog to offer
    // Force.
    await beginTeardown(FIXTURE.dbHostId, 1_000);
    await simulateDecommission({ serverId: FIXTURE.dbHostId, event: "timeout" });

    const row = await serverRow(FIXTURE.dbHostId);
    expect(row, "the timeout simulation must not tombstone the server").toBeDefined();
    expect(row.status).toBe(SERVER_STATUS.decommissioning);
    const age = Date.now() - new Date(row.decommissionStartedAt!).getTime();
    expect(age).toBeGreaterThanOrEqual(DECOMMISSION_TIMEOUT_MS);
  });

  // The half the status check cannot see. A page arriving late finds a row that
  // IS decommissioning, so status alone waves it through — and adversarial
  // review drove exactly that shape and watched the server get deleted.
  it("refuses to confirm a teardown nothing answered inside the window, and points at Force disconnect", async () => {
    await beginTeardown(FIXTURE.dbHostId, DECOMMISSION_TIMEOUT_MS + 60_000);
    await expect(
      simulateDecommission({ serverId: FIXTURE.dbHostId, event: "ack" })
    ).rejects.toThrow(/Force disconnect/i);
    expect(await serverRow(FIXTURE.dbHostId)).toBeDefined();
  });

  it("still lets the timeout simulation run on a row that is already past the window", async () => {
    // Ageing a row past the window is that button's whole job, so the guard
    // above must not catch it — a guard that made its own demonstration
    // unreachable would be the previous bug wearing a badge.
    await beginTeardown(FIXTURE.dbHostId, DECOMMISSION_TIMEOUT_MS + 60_000);
    await simulateDecommission({ serverId: FIXTURE.dbHostId, event: "timeout" });
    expect(await serverRow(FIXTURE.dbHostId)).toBeDefined();
  });

  it("lets the silence simulation touch a server that is not being decommissioned", async () => {
    // "silence" is not a report ABOUT a teardown — it is the agent going quiet,
    // which happens to any host at any time. Sharing the guard with the other
    // three would have made an ordinary state unreachable.
    await simulateDecommission({ serverId: FIXTURE.dbHostId, event: "silence" });
    const row = await serverRow(FIXTURE.dbHostId);
    expect(row).toBeDefined();
    expect(row.status).toBe(SERVER_STATUS.unreachable);
  });

  it("does nothing at all when a control plane is in charge", async () => {
    // Every CP export in this file throws, so reaching one would fail loudly.
    // What is pinned is the early return: in CP mode a real sigmad answers, or
    // does not, and this action has no business writing anything.
    const mod = await import("@/server/cp");
    vi.spyOn(mod, "cpEnabled").mockReturnValue(true);
    await beginTeardown(FIXTURE.dbHostId, 1_000);
    await simulateDecommission({ serverId: FIXTURE.dbHostId, event: "ack" });
    expect(await serverRow(FIXTURE.dbHostId)).toBeDefined();
    vi.restoreAllMocks();
  });
});

// SIGMA-229. The bound-resources guard has to ask the same question the
// renderer asks — "what runs here?" — and a cluster workload runs on a node
// without ever naming it: it binds to the CLUSTER, so `resources.server_id` is
// null and the narrow check saw an empty host. Demo mode therefore offered the
// clean graceful teardown for a node that was still running the cluster.
describe("what counts as bound to a demo host", () => {
  async function seedClusterWorkload() {
    await database.insert(s.clusters).values({
      id: "cls_prod",
      orgId: FIXTURE.orgId,
      environmentId: FIXTURE.prodEnvId,
      name: "production",
    });
    await database.insert(s.clusterNodes).values([
      { clusterId: "cls_prod", serverId: FIXTURE.k8sHostIds[0], role: "control-plane" },
      { clusterId: "cls_prod", serverId: FIXTURE.k8sHostIds[1], role: "worker" },
    ]);
    await database.insert(s.resources).values({
      id: "res_cluster_app",
      projectId: FIXTURE.projectId,
      environmentId: FIXTURE.prodEnvId,
      clusterId: "cls_prod",
      name: "checkout",
      kind: "app",
    });
  }

  it("refuses to disconnect a cluster node that still runs the cluster's workloads", async () => {
    await seedClusterWorkload();
    const res = await decommissionServer({ serverId: FIXTURE.k8sHostIds[1] });
    if (res.ok) {
      throw new Error("a node carrying cluster workloads was offered a clean teardown");
    }
    expect(res.boundResources).toEqual(["checkout"]);
    // Nothing was started: the refusal is complete.
    expect((await serverRow(FIXTURE.k8sHostIds[1])).status).toBe(SERVER_STATUS.running);
  });

  it("still lets a host with nothing on it go", async () => {
    await seedClusterWorkload();
    // Node 3 joined no cluster and owns no resource — the widened guard must
    // not become a refusal that never lifts.
    const res = await decommissionServer({ serverId: FIXTURE.k8sHostIds[2] });
    expect(res.ok).toBe(true);
    expect((await serverRow(FIXTURE.k8sHostIds[2])).status).toBe(SERVER_STATUS.decommissioning);
  });
});

// SIGMA-300. provisionServer's CP branch is a mutation followed by a render,
// and the render can refuse: the install command will not be built over a
// plaintext public URL, nor for a control plane pinned to no release. Both
// refusals used to arrive AFTER cpProvisionServer had already pre-created the
// server and burned a bootstrap keypair, so the operator saw only a red toast,
// assumed a typo, and pressed Connect again — leaving a column of identical
// hosts stuck in Provisioning, each of which has to be disconnected by hand.
//
// The invariant is stated as an outcome rather than as a call order, because
// there are two honest ways to hold it: refuse before the mutation, or undo the
// mutation on the way out. What must never happen is a server row surviving a
// throw.
describe("provisioning a server whose install command cannot be rendered", () => {
  const PROVISIONED = {
    serverId: "srv_ghost",
    token: "sbt_ghost",
    expiresAt: new Date(Date.now() + 900_000).toISOString(),
    bootstrapPubkey: "ssh-ed25519 AAAA sigmahub-bootstrap",
    agentVersion: "v0.3.0",
  };

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("leaves no server behind when the control plane's public URL is plaintext", async () => {
    const mod = await import("@/server/cp");
    vi.spyOn(mod, "cpEnabled").mockReturnValue(true);
    // The address the deployment guide itself documented.
    vi.spyOn(mod, "cpPublicUrl").mockReturnValue("http://cp:8080");
    const provision = vi.spyOn(mod, "cpProvisionServer").mockResolvedValue(PROVISIONED);
    const remove = vi.spyOn(mod, "cpDeleteServer").mockResolvedValue(undefined);

    await expect(
      provisionServer({ orgId: FIXTURE.orgId, type: "general", hostIp: "203.0.113.7" })
    ).rejects.toThrow(/https/i);

    // Either the refusal came first and nothing was created, or something was
    // created and taken back. A server that outlives the throw is the defect.
    expect(
      provision.mock.calls.length > 0 && remove.mock.calls.length === 0,
      "the TLS refusal left a provisioned server behind — the operator sees only a red toast, " +
        "presses Connect again, and collects a column of Provisioning rows with burned tokens"
    ).toBe(false);
  });

  it("leaves no server behind when the control plane is pinned to no release", async () => {
    // This one genuinely cannot be known before the call — the version comes
    // back with the token — so the only way to hold the invariant is to undo.
    const mod = await import("@/server/cp");
    vi.spyOn(mod, "cpEnabled").mockReturnValue(true);
    vi.spyOn(mod, "cpPublicUrl").mockReturnValue("https://cp.example.com");
    vi.spyOn(mod, "cpProvisionServer").mockResolvedValue({
      ...PROVISIONED,
      agentVersion: "",
      agentVersionError: "CP_AGENT_VERSION is not a released tag",
    });
    const remove = vi.spyOn(mod, "cpDeleteServer").mockResolvedValue(undefined);

    await expect(
      provisionServer({ orgId: FIXTURE.orgId, type: "general", hostIp: "203.0.113.7" })
    ).rejects.toThrow(/CP_AGENT_VERSION/);

    expect(remove).toHaveBeenCalledWith(FIXTURE.orgId, PROVISIONED.serverId, expect.anything());
  });
});
