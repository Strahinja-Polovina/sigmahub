import type { ServerType, ResourceKind } from "@/lib/mock";

// Resource-type × server-type availability matrix (SIGMA-A-3 §2). Pure domain
// configuration — which server class can host which kind of resource.
export const availabilityMatrix: Record<ServerType, ResourceKind[]> = {
  general: ["app", "postgres", "mysql", "mongo", "redis"],
  database: ["postgres", "mysql", "mongo", "redis"],
  storage: ["s3"],
  gpu: ["llm", "app"],
};

export function canHost(server: ServerType, kind: ResourceKind) {
  return availabilityMatrix[server].includes(kind);
}
