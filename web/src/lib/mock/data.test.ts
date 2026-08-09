/**
 * The demo dataset, checked against the flows it has to be able to WALK
 * (SIGMA-215).
 *
 * Demo mode is not test scaffolding: with no control plane configured it is the
 * whole product, and it is how a prospective user sees a cluster, a GPU fit
 * check or a decommission without owning the hardware. So the fixtures carry a
 * job, and the job is checkable — every assertion below is "this screen is
 * reachable offline", stated as a property of the data rather than as a
 * screenshot somebody remembers taking.
 *
 * Each verdict is computed by CALLING the product's own code — the compatibility
 * gate, the fit check, the disconnect dialog's clock — never by matching the
 * sentences they produce. A fixture that drifts from the product then fails
 * here, which is the only place it can fail before a demo.
 */

import { describe, expect, it } from "vitest";
import { clusters, environments, projects, resources, servers } from "./data";
import { findMockModel, MOCK_MODELS } from "./models";
import { checkServerCompatibility, SERVER_STATUS } from "@/lib/server-compat";
import { clusterCanHost } from "@/lib/server-catalog.generated";
import {
  DECOMMISSION_TIMEOUT_MS,
  demoTeardownPhase,
  demoTeardownSpanMs,
  forceReason,
} from "@/lib/decommission";
import {
  controlPlaneRefusal,
  demoApiEndpoint,
  demoClusterStatus,
  demoNodeReport,
} from "@/lib/demo-cluster";
import { serverFitsModel } from "@/lib/wizard/llm";

const serverById = new Map(servers.map((sv) => [sv.id, sv]));

describe("the demo fleet holds together", () => {
  it("attaches every environment to servers that exist, in the same org", () => {
    for (const env of environments) {
      const orgId = projects.find((p) => p.id === env.projectId)?.orgId;
      for (const id of env.serverIds) {
        expect(serverById.get(id), `${env.id} attaches unknown server ${id}`).toBeDefined();
        expect(serverById.get(id)!.orgId).toBe(orgId);
      }
    }
  });

  // The two directions of the same attachment are both fixture data, and a
  // dashboard reading one while the seed writes the other would show a server
  // in an environment the environment does not list.
  it("agrees with itself about which environments a server is in", () => {
    for (const sv of servers) {
      const fromEnvs = environments
        .filter((e) => e.serverIds.includes(sv.id))
        .map((e) => e.id)
        .sort();
      expect([...sv.environmentIds].sort(), `${sv.name}`).toEqual(fromEnvs);
    }
  });

  it("counts each server's resources the way the dashboard will", () => {
    for (const sv of servers) {
      const actual = resources.filter((r) => r.serverId === sv.id).length;
      expect(sv.resourceCount, `${sv.name}`).toBe(actual);
    }
  });

  // `ip` is the PUBLIC address and meshIp the private WireGuard one; the
  // dashboard showed the second under the first's heading until SIGMA-187, and
  // a fixture that filled the public column with a 10.8.x.x address would put
  // that bug straight back on the demo's front page.
  it("keeps the public address and the mesh address apart", () => {
    for (const sv of servers) {
      expect(sv.ip.startsWith("10."), `${sv.name} publishes a mesh address as its IP`).toBe(false);
      if (sv.meshIp) expect(sv.meshIp.startsWith("10.")).toBe(true);
    }
  });
});

describe("a host the compatibility gate refuses", () => {
  const verdicts = servers.map((sv) => ({
    server: sv,
    reasons: checkServerCompatibility(sv.type, sv.facts ?? {}),
  }));

  // Without one, the `incompatible` status, its badge, its explanation and both
  // of its exits are unreachable offline — and nobody demoing the product owns
  // a GPU box with no GPU in it.
  it("is in the fleet, and says which requirement the machine failed", () => {
    const refused = verdicts.filter((v) => v.reasons.length > 0);
    expect(refused.map((v) => v.server.name)).toEqual(["fsn-gpu-02"]);
    expect(refused[0].reasons[0].id).toBe("gpu");
    expect(refused[0].reasons[0].reason).toContain("GPU server");
  });

  // The gate must be the thing that decided it. A fixture that also wrote
  // `status: "incompatible"` down would keep reading incompatible after someone
  // fixed the facts, which is the drift deriving it exists to prevent.
  it("is the only host refused, and none of them claims the verdict for itself", () => {
    for (const { server, reasons } of verdicts) {
      expect(server.status, `${server.name}`).not.toBe(SERVER_STATUS.incompatible);
      if (server.name !== "fsn-gpu-02") expect(reasons, `${server.name}`).toEqual([]);
    }
  });
});

// Two clocks read the same seeded timestamp, and this fixture used to be
// checked against one of them: the control plane's ten-minute patience said
// "still working" at two minutes, so the test passed, while the servers page
// measured the same row against the demo teardown's ten SECONDS, read it as
// finished, wrote the agent's ack and deleted the server on first paint. Every
// assertion here therefore names WHICH clock it is asking.
describe("a decommission in flight", () => {
  const teardowns = servers.filter((sv) => sv.decommission);
  const seeded = () => {
    const { startedMsAgo, purgeVolumes } = teardowns[0].decommission!;
    return { startedAt: new Date(Date.now() - startedMsAgo), purgeVolumes };
  };

  it("is in the fleet, on a host with nothing left running on it", () => {
    expect(teardowns).toHaveLength(1);
    // decommissionServer refuses a host with bound resources, so a fixture that
    // had any could not have reached this state through the product.
    expect(resources.filter((r) => r.serverId === teardowns[0].id)).toEqual([]);
  });

  // The state a seeded row can actually hold, asserted as the state it claims.
  //
  // The value in between the two clocks reads better and is not reachable: the
  // PGlite directory is seeded once at build time and the control plane's
  // window is ten minutes, so every visitor after the first ten minutes of a
  // deployment's life opens past it regardless. Pinning the reachable state is
  // what keeps the fixture's comment true a week after it was written.
  it("opens on a teardown nothing ever answered, which is the state with somewhere to go", () => {
    const input = {
      status: SERVER_STATUS.decommissioning,
      decommissioningSince: seeded().startedAt,
    };
    // Force disconnect and the manual cleanup script — the payoff no timer
    // produces, and the reason this row is in the fixture at all.
    expect(forceReason(input)?.kind).toBe("timedOut");
    // And it is past the window by construction rather than by luck: the age is
    // derived from DECOMMISSION_TIMEOUT_MS, so editing that constant moves the
    // fixture with it instead of stranding it on the wrong side.
    expect(forceReason({ ...input, now: Date.now() - DECOMMISSION_TIMEOUT_MS })).toBeNull();
  });

  // The clock the old test never asked, and the answer it has to give. A seeded
  // database is opened minutes or days after it was written, so the ten seconds
  // of absolute wall time this fixture would have to be inside are long gone —
  // no offset can put a visitor there, and one small enough to try would only
  // make it a race, where whoever loads the page quickly enough watches an
  // unrequested server vanish. The demo's ticking step counter belongs to the
  // teardown the visitor starts and the page therefore watches from step zero;
  // this row's job is the durable half, and the page reads it as what it is —
  // asked for, unconfirmed, still inside the window.
  it("is past the demo teardown's own clock, which is why the page must not read it as acked", () => {
    expect(demoTeardownPhase(seeded()).done).toBe(true);
  });

  it("clears both clocks by construction, not by a number that fits today", () => {
    const { startedMsAgo } = teardowns[0].decommission!;
    // The longest sequence, so the bound holds whichever volume choice a
    // fixture makes.
    expect(startedMsAgo, "a visitor could arrive mid-teardown and ack a server nobody touched")
      .toBeGreaterThan(demoTeardownSpanMs(true));
    // And past the control plane's window as well. The value in between reads
    // better and cannot be held: the demo database is seeded once at build time
    // and the window is ten minutes, so a visitor an hour into a deployment's
    // life sees the force path whatever the fixture intended. Pinning the
    // reachable state is what stops the comment beside it from going stale.
    expect(startedMsAgo, "the seeded state would depend on how long ago the build ran")
      .toBeGreaterThan(DECOMMISSION_TIMEOUT_MS);
  });

  // The route SIGMA-205 exists for: timeout → Force disconnect → cleanup
  // script. The button that starts it backdates the row past the control
  // plane's window, at which point the demo clock also reads finished — and a
  // page that treats "finished" as "the agent reported" acks it and DELETES the
  // server, which is the one outcome this path must not have. The button's own
  // toast promises the operator the dialog will offer Force disconnect.
  it("lands in the force path when the agent never answers, never in deletion", () => {
    // What simulateDecommission's "timeout" branch writes (actions/servers.ts).
    const backdated = new Date(Date.now() - DECOMMISSION_TIMEOUT_MS - 60_000);
    expect(
      forceReason({
        status: SERVER_STATUS.decommissioning,
        decommissioningSince: backdated,
      })?.kind
    ).toBe("timedOut");
    expect(demoTeardownPhase({ startedAt: backdated, purgeVolumes: false }).done).toBe(true);
  });

  // Named volumes are the customer's database directories and uploaded files.
  // Off is the default at every other layer, and a demo that opened on the
  // destructive choice would teach the wrong one.
  it("keeps the customer's volumes, which is the default everywhere else", () => {
    expect(teardowns[0].decommission!.purgeVolumes).toBe(false);
  });
});

describe("the cluster", () => {
  const cluster = clusters[0];

  it("exists, in an environment that already has servers and resources of its own", () => {
    expect(clusters).toHaveLength(1);
    const env = environments.find((e) => e.id === cluster.environmentId)!;
    expect(env.serverIds.length).toBeGreaterThan(0);
    expect(resources.filter((r) => r.environmentId === env.id).length).toBeGreaterThan(1);
  });

  it("has one control plane and at least one worker, all of them real servers", () => {
    const controlPlane = cluster.nodes.filter((n) => n.role === "control-plane");
    const workers = cluster.nodes.filter((n) => n.role === "worker");
    expect(controlPlane).toHaveLength(1);
    expect(workers.length).toBeGreaterThanOrEqual(1);
    for (const node of cluster.nodes) {
      const sv = serverById.get(node.serverId);
      expect(sv, `unknown node ${node.serverId}`).toBeDefined();
      expect(sv!.orgId).toBe(cluster.orgId);
    }
  });

  // controlPlaneRefusal turns away any host that is not running, for both the
  // promote and the join path, so a node seeded over one would be a cluster the
  // product refuses to build.
  it("is built only from servers the create dialog would have accepted", () => {
    for (const node of cluster.nodes) {
      expect(controlPlaneRefusal(serverById.get(node.serverId)!)).toBeNull();
    }
  });

  // Read back, everything about this cluster is recomputed from its hosts, so
  // the assertion has to be made through the same functions the panel uses.
  // Anything else would be checking a fixture against itself.
  it("reads ready, which is what keeps it a deploy target the wizard can offer", () => {
    const reports = cluster.nodes.map((n) => ({
      role: n.role,
      status: demoNodeReport({
        joinedAt: new Date(Date.now() - n.joinedDaysAgo * 86_400_000),
        serverStatus: serverById.get(n.serverId)!.status,
      }).status,
    }));
    expect(demoClusterStatus(reports)).toBe("ready");
  });

  // Its API server answers on the control-plane node's mesh address, never on a
  // public interface, which is the promise the create dialog makes — and the
  // endpoint renders empty for a control plane with no mesh address at all.
  it("publishes its API server on the control-plane node's mesh address", () => {
    const controlPlane = serverById.get(
      cluster.nodes.find((n) => n.role === "control-plane")!.serverId
    )!;
    expect(demoApiEndpoint(controlPlane.meshIp)).toBe(`https://${controlPlane.meshIp}:6443`);
  });
});

describe("every seeded resource is one the product would have created", () => {
  it("targets exactly one of a server and a cluster", () => {
    for (const r of resources) {
      expect([r.serverId, r.clusterId].filter(Boolean), `${r.name}`).toHaveLength(1);
    }
  });

  // The exclusion list is the control plane's, published through the generated
  // catalog. Restating it here would be a second copy to keep in step, which is
  // the defect the generated catalog removed.
  it("never puts a kind inside a cluster that the control plane refuses to schedule there", () => {
    for (const r of resources) {
      if (r.clusterId) expect(clusterCanHost(r.kind), `${r.name} is a ${r.kind}`).toBe(true);
    }
  });

  it("puts at least one workload in the cluster and leaves the stateful ones outside it", () => {
    const env = clusters[0].environmentId;
    const inside = resources.filter((r) => r.clusterId === clusters[0].id);
    const outside = resources.filter((r) => r.environmentId === env && !r.clusterId);
    expect(inside.length).toBeGreaterThan(0);
    // The panel's whole argument: the app runs in the cluster and reaches a
    // database that does not.
    expect(outside.some((r) => !clusterCanHost(r.kind))).toBe(true);
  });
});

describe("the GPU fleet demonstrates the VRAM fit check", () => {
  const gpuHosts = servers.filter((sv) => (sv.facts?.gpu?.vramBytesPerGpu ?? 0) > 0);

  it("reports more than one card size, so the check has something to disagree about", () => {
    const sizes = new Set(gpuHosts.map((sv) => sv.facts!.gpu!.vramBytesPerGpu));
    expect(gpuHosts.length).toBeGreaterThanOrEqual(2);
    expect(sizes.size).toBe(gpuHosts.length);
  });

  // The fit check's entire argument is that the number on the box approves
  // models the card cannot hold. A fixture written from the marketing figure
  // would demo a check that never says no — so every figure here is a whole
  // number of MiB (what nvidia-smi enumerates) and none of them is a round
  // number of decimal gigabytes (what the invoice says).
  it("states each card's memory as an enumerated size and not as the number on the box", () => {
    for (const sv of gpuHosts) {
      const bytes = sv.facts!.gpu!.vramBytesPerGpu!;
      expect(bytes % (1024 * 1024), `${sv.name} is not a whole number of MiB`).toBe(0);
      expect(bytes % 1_000_000_000, `${sv.name} is a marketing figure`).not.toBe(0);
    }
  });

  it("holds a model each card refuses and a model each card takes", () => {
    for (const sv of gpuHosts) {
      const facts = { gpu: sv.facts!.gpu };
      expect(MOCK_MODELS.some((m) => serverFitsModel(m, facts).fits), sv.name).toBe(true);
      expect(MOCK_MODELS.some((m) => !serverFitsModel(m, facts).fits), sv.name).toBe(true);
    }
  });

  // The demo used to run a 70B checkpoint on a 40 GB A100 — 188 GB of weights
  // on a card that holds 42 — so the seeded fleet contradicted the fit check on
  // the screen beside it. The pairing is written down because the mirror stores
  // no model id: what the endpoint serves lives in the resource's CP spec, and
  // demo mode has none.
  const ENDPOINT_MODELS: Record<string, string> = {
    "llama-3-8b": "meta-llama/Llama-3.1-8B-Instruct",
    "llama-3-70b-awq": "hugging-quants/Meta-Llama-3.1-70B-Instruct-AWQ-INT4",
    "qwen2-5-7b": "Qwen/Qwen2.5-7B-Instruct",
  };

  it("runs no model endpoint on a card the wizard would have refused it for", () => {
    const endpoints = resources.filter((r) => r.kind === "llm");
    expect(endpoints.map((r) => r.name).sort()).toEqual(Object.keys(ENDPOINT_MODELS).sort());
    for (const endpoint of endpoints) {
      const model = findMockModel(ENDPOINT_MODELS[endpoint.name]);
      const host = serverById.get(endpoint.serverId!)!;
      expect(model, `${endpoint.name} names a model the catalogue does not have`).toBeDefined();
      expect(serverFitsModel(model, { gpu: host.facts?.gpu }), `${endpoint.name} on ${host.name}`)
        .toEqual({ fits: true });
    }
  });
});
