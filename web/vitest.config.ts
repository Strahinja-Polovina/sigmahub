import { defineConfig } from "vitest/config";
import path from "node:path";

export default defineConfig({
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "src"),
      // Component tests reach client components whose server-action modules
      // begin with `import "server-only"`. That specifier is resolved by the
      // Next compiler and is not a real dependency, so Vitest cannot find it.
      // See src/test/server-only.ts.
      "server-only": path.resolve(__dirname, "src/test/server-only.ts"),
    },
  },
  test: {
    include: ["src/**/*.test.{ts,tsx}"],
    environment: "node",
  },
});
