import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Keep PGlite out of the server bundle — bundling breaks its WASM/filesystem
  // path resolution (ERR_INVALID_ARG_TYPE). Loaded from node_modules at runtime.
  // In prod the DB swaps to AlloyDB/Cloud SQL and this is a no-op.
  serverExternalPackages: ["@electric-sql/pglite"],
};

export default nextConfig;
