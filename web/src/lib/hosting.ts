import type { ServerType, ResourceKind } from "@/lib/mock";

// Resource-kind × server-type availability matrix. Pure domain configuration,
// mirrored from the control plane's resourceServerTypes (a test asserts the two
// agree) — which server class can host which kind of resource.
//
// The rules behind it:
//   - "vps" runs whatever "general" runs; virtualization is a disclosure, not a
//     capability difference.
//   - "k8s" and "build" host nothing directly. Cluster workloads arrive through
//     the control plane, and a build server compiles images and ships them.
//   - "llm" needs a GPU: serving a model on CPU is possible and useless.
export const availabilityMatrix: Record<ServerType, ResourceKind[]> = {
  general: ["app", "postgres", "mysql", "mongo", "redis"],
  vps: ["app", "postgres", "mysql", "mongo", "redis"],
  database: ["postgres", "mysql", "mongo", "redis"],
  storage: ["s3"],
  gpu: ["llm", "app"],
  k8s: [],
  build: [],
};

export function canHost(server: ServerType, kind: ResourceKind) {
  return (availabilityMatrix[server] ?? []).includes(kind);
}

/** Why a server type can't host anything, for an honest empty state. */
export const HOSTS_NOTHING_REASON: Partial<Record<ServerType, string>> = {
  k8s: "Cluster nodes receive workloads from the cluster's control plane, not directly.",
  build: "Build servers compile images and push them to a registry; they run no workloads.",
};
