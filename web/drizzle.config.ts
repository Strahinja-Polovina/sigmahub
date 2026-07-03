import { defineConfig } from "drizzle-kit";

export default defineConfig({
  dialect: "postgresql",
  schema: ["./src/server/db/schema.ts", "./src/server/db/auth-schema.ts"],
  out: "./drizzle",
});
