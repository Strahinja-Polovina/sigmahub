import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Keep PGlite out of the server bundle — bundling breaks its WASM/filesystem
  // path resolution (ERR_INVALID_ARG_TYPE). Loaded from node_modules at runtime.
  // In prod the DB swaps to any Postgres via DATABASE_URL and this is a no-op.
  serverExternalPackages: ["@electric-sql/pglite"],
  // Self-contained server output for the container image (web/Dockerfile).
  output: "standalone",
  // Baseline security headers on a hosted dashboard that holds sessions and
  // reveals DB/S3 secrets (SIGMA-365). Deliberately no script/style CSP directives
  // — those need per-response nonces to not break Next's inline runtime — but
  // frame-ancestors + X-Frame-Options close clickjacking, HSTS prevents downgrade,
  // and nosniff/referrer are free hardening. Applied to every route.
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Content-Security-Policy", value: "frame-ancestors 'none'" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
          {
            key: "Strict-Transport-Security",
            value: "max-age=31536000; includeSubDomains",
          },
        ],
      },
    ];
  },
};

export default nextConfig;
