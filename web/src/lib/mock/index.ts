// Mock data retired (V1-7). The dashboard now reads real, persisted data via
// `@/server/queries` and mutates through server actions. This module survives
// only as the re-export point for the shared domain *types*, which are now
// generated from the control plane's catalog (@/lib/server-catalog.generated).
// The fixture arrays in ./data are used solely by the DB seed
// (src/server/db/seed.ts) to bootstrap a fresh database; the hosting matrix
// moved into the generated catalog and the rest into @/lib/sample-telemetry.
export * from "./types";
