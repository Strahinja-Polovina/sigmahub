// Next.js server-startup hook: runs once per server boot (nodejs runtime).
// Production (DATABASE_URL set) applies drizzle migrations before serving —
// the container needs no separate migrate step. Demo/PGlite boots do nothing
// here (the seed script owns dev schema).
export async function register() {
  if (process.env.NEXT_RUNTIME !== "nodejs") return;

  // Configuration that must fail the CONTAINER, not the first request that
  // happens to touch it (SIGMA-365).
  //
  // configuredMailTransport() throws on an SMTP_FROM with no addr-spec, and that
  // was described as failing at boot. It does not: nothing imports lib/mail until
  // a route does, so the container started, `GET /api/health?require=cp` — the
  // probe the staging rollout gates on — answered 200 because it touches neither
  // mail nor auth, the deploy was marked healthy, and every page that renders the
  // auth layout 500'd. A validation whose failure is invisible to the health
  // check is not a boot check; this is where boot actually happens.
  const { configuredMailTransport } = await import("./lib/mail");
  configuredMailTransport();

  if (!process.env.DATABASE_URL) return;
  const { migrateProd } = await import("./server/db/migrate-prod");
  await migrateProd();
  console.info("[db] migrations applied");
}
