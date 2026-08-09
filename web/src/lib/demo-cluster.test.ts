import { describe, expect, it } from "vitest";

import {
  CLUSTER_STATUS,
  DEMO_KUBERNETES_VERSION,
  DEMO_NODE_READY_MS,
  NODE_ROLE_CONTROL_PLANE,
  NODE_ROLE_WORKER,
  NODE_STATUS,
  controlPlaneRefusal,
  demoApiEndpoint,
  demoClusterStatus,
  demoKubernetesVersion,
  demoNodeReport,
  msUntilNodeReady,
} from "./demo-cluster";
import { SERVER_STATUS } from "./server-compat";

const NOW = Date.UTC(2027, 4, 1, 12, 0, 0);
const ago = (ms: number) => new Date(NOW - ms);

describe("a demo node's report about Kubernetes", () => {
  it("is pending while the install is still running", () => {
    expect(
      demoNodeReport({
        joinedAt: ago(DEMO_NODE_READY_MS - 1),
        serverStatus: SERVER_STATUS.running,
        now: NOW,
      }).status
    ).toBe(NODE_STATUS.pending);
  });

  it("is ready once the install has had its time", () => {
    expect(
      demoNodeReport({
        joinedAt: ago(DEMO_NODE_READY_MS),
        serverStatus: SERVER_STATUS.running,
        now: NOW,
      }).status
    ).toBe(NODE_STATUS.ready);
  });

  // The whole point of consulting the SERVER's status first: a node on a host
  // nobody can reach has not installed anything, however long ago it joined.
  it("is an error on a host that stopped answering, however long ago it joined", () => {
    const report = demoNodeReport({
      joinedAt: ago(30 * 86_400_000),
      serverStatus: SERVER_STATUS.unreachable,
      now: NOW,
    });
    expect(report.status).toBe(NODE_STATUS.error);
    expect(report.message).not.toBe("");
  });

  it("is an error on a host the enrollment gate refused, and says which", () => {
    const report = demoNodeReport({
      joinedAt: ago(DEMO_NODE_READY_MS * 10),
      serverStatus: SERVER_STATUS.incompatible,
      now: NOW,
    });
    expect(report.status).toBe(NODE_STATUS.error);
    expect(report.message).toContain("server type");
  });

  it("is pending on a host whose agent has not checked in at all", () => {
    expect(
      demoNodeReport({
        joinedAt: ago(DEMO_NODE_READY_MS * 10),
        serverStatus: SERVER_STATUS.provisioning,
        now: NOW,
      }).status
    ).toBe(NODE_STATUS.pending);
  });

  // A seeded row may carry no join time at all; the alternative to answering
  // "ready" is a node that is pending forever with nothing that could move it.
  it("is ready when there is no join time to wait from", () => {
    expect(
      demoNodeReport({ joinedAt: null, serverStatus: SERVER_STATUS.running, now: NOW }).status
    ).toBe(NODE_STATUS.ready);
  });
});

describe("waiting for a demo node", () => {
  it("reports the time left while the install is running", () => {
    expect(
      msUntilNodeReady({
        joinedAt: ago(2_000),
        serverStatus: SERVER_STATUS.running,
        now: NOW,
      })
    ).toBe(DEMO_NODE_READY_MS - 2_000);
  });

  it("reports nothing to wait for once the node is ready", () => {
    expect(
      msUntilNodeReady({
        joinedAt: ago(DEMO_NODE_READY_MS + 1),
        serverStatus: SERVER_STATUS.running,
        now: NOW,
      })
    ).toBeNull();
  });

  // Not a deadline: the node recovers when the host does, which is an event.
  it("reports nothing to wait for on a host that is not running", () => {
    expect(
      msUntilNodeReady({
        joinedAt: ago(1_000),
        serverStatus: SERVER_STATUS.unreachable,
        now: NOW,
      })
    ).toBeNull();
  });
});

describe("a demo cluster's status, derived from its nodes", () => {
  const cp = (status: string) => ({ role: NODE_ROLE_CONTROL_PLANE, status });
  const worker = (status: string) => ({ role: NODE_ROLE_WORKER, status });

  it("is provisioning until the control plane itself reports ready", () => {
    expect(demoClusterStatus([cp(NODE_STATUS.pending), worker(NODE_STATUS.ready)])).toBe(
      CLUSTER_STATUS.provisioning
    );
  });

  it("is ready when the control plane and every worker reported ready", () => {
    expect(demoClusterStatus([cp(NODE_STATUS.ready), worker(NODE_STATUS.ready)])).toBe(
      CLUSTER_STATUS.ready
    );
  });

  it("is degraded when the control plane is up and a worker is not", () => {
    expect(demoClusterStatus([cp(NODE_STATUS.ready), worker(NODE_STATUS.error)])).toBe(
      CLUSTER_STATUS.degraded
    );
  });

  // "Every node is ready" is vacuously true of nothing, and a cluster badge
  // reading `ready` over an empty node list means nothing at all.
  it("is provisioning, not ready, when there are no nodes", () => {
    expect(demoClusterStatus([])).toBe(CLUSTER_STATUS.provisioning);
  });
});

describe("what a demo cluster publishes", () => {
  it("serves its API on the control-plane node's MESH address", () => {
    expect(demoApiEndpoint("10.8.0.21")).toBe("https://10.8.0.21:6443");
  });

  // An address the reader could try to curl would be worse than none.
  it("publishes no endpoint for a control plane with no mesh address", () => {
    expect(demoApiEndpoint("")).toBe("");
    expect(demoApiEndpoint(null)).toBe("");
  });

  it("reports no Kubernetes version until the control plane is actually serving", () => {
    expect(demoKubernetesVersion(CLUSTER_STATUS.provisioning)).toBe("");
    expect(demoKubernetesVersion(CLUSTER_STATUS.ready)).toBe(DEMO_KUBERNETES_VERSION);
    expect(demoKubernetesVersion(CLUSTER_STATUS.degraded)).toBe(DEMO_KUBERNETES_VERSION);
  });
});

describe("promoting a server into a cluster control plane", () => {
  it("accepts a running host", () => {
    expect(controlPlaneRefusal({ name: "fsn-01", status: SERVER_STATUS.running })).toBeNull();
  });

  // Each refusal names the host and the fix; "not eligible" would send the
  // reader back to the servers page to work out which of five states it is in.
  it.each([
    [SERVER_STATUS.provisioning, "checked in"],
    [SERVER_STATUS.unreachable, "answering"],
    [SERVER_STATUS.incompatible, "server type"],
    [SERVER_STATUS.decommissioning, "disconnected"],
  ])("refuses a %s host, naming it and what to do", (status, fragment) => {
    const refusal = controlPlaneRefusal({ name: "fsn-01", status });
    expect(refusal).toContain("fsn-01");
    expect(refusal).toContain(fragment);
  });

  it("refuses a status it has never heard of rather than accepting it", () => {
    expect(controlPlaneRefusal({ name: "fsn-01", status: "quarantined" })).not.toBeNull();
  });
});
