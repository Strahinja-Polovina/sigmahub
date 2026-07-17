// Next.js server-startup hook: runs once per server boot (nodejs runtime).
// Production (DATABASE_URL set) applies drizzle migrations before serving —
// the container needs no separate migrate step. Demo/PGlite boots do nothing
// here (the seed script owns dev schema).
export async function register() {
  if (process.env.NEXT_RUNTIME !== "nodejs" || !process.env.DATABASE_URL) return;
  const { migrateProd } = await import("./server/db/migrate-prod");
  await migrateProd();
  console.info("[db] migrations applied");
}
