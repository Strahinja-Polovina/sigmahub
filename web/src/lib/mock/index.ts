// Mock data retired (V1-7). The dashboard now reads real, persisted data via
// `@/server/queries` and mutates through server actions. This module survives
// only as the single source of shared domain *types*; the fixture arrays in
// ./data are used solely by the DB seed (src/server/db/seed.ts) to bootstrap a
// fresh database, and the pure helpers moved to @/lib/hosting and
// @/lib/sample-telemetry.
export * from "./types";
