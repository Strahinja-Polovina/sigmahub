import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Keep PGlite out of the server bundle — bundling breaks its WASM/filesystem
  // path resolution (ERR_INVALID_ARG_TYPE). Loaded from node_modules at runtime.
  // In prod the DB swaps to any Postgres via DATABASE_URL and this is a no-op.
  serverExternalPackages: ["@electric-sql/pglite"],
  // Self-contained server output for the container image (web/Dockerfile).
  output: "standalone",
};

export default nextConfig;
