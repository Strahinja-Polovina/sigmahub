import { describe, expect, it } from "vitest";
import {
  envVarCount,
  envVarsValid,
  isSecretKey,
  mergeEnvVars,
  parseDotenv,
  seedEnvVars,
  submittableEnvVars,
} from "./env";

describe("keys are seeded from what the repository declares", () => {
  it("keeps the detected keys, blanks the values, and leaves a row to type in", () => {
    const seeded = seedEnvVars(["DATABASE_URL", "LOG_LEVEL"]);
    expect(seeded.map((v) => v.key)).toEqual(["DATABASE_URL", "LOG_LEVEL", ""]);
    expect(seeded.every((v) => v.value === "")).toBe(true);
  });

  it("drops duplicates and names the server would reject", () => {
    const seeded = seedEnvVars(["A", "A", "9BAD", "with space", "  B  "]);
    expect(seeded.map((v) => v.key)).toEqual(["A", "B", ""]);
  });
});

describe("credentials are marked", () => {
  it("marks the obvious ones", () => {
    for (const key of [
      "DATABASE_PASSWORD",
      "SESSION_SECRET",
      "GITHUB_TOKEN",
      "STRIPE_API_KEY",
      "PRIVATE_KEY",
      "DATABASE_DSN",
      "SIGNING_CERT",
    ]) {
      expect(isSecretKey(key), key).toBe(true);
    }
  });

  // "_KEY" is matched as a SUFFIX rather than a substring: KEYSPACE and
  // KEYCLOAK_URL are not credentials, and a heuristic that masks them teaches
  // people to ignore the mask.
  it("does not mark things that merely contain the word", () => {
    for (const key of ["KEYSPACE", "KEYCLOAK_URL", "MONKEY_MODE", "LOG_LEVEL", "PORT"]) {
      expect(isSecretKey(key), key).toBe(false);
    }
  });

  it("seeds the mark from the key", () => {
    const seeded = seedEnvVars(["DB_PASSWORD", "LOG_LEVEL"]);
    expect(seeded[0].secret).toBe(true);
    expect(seeded[1].secret).toBe(false);
  });
});

describe("pasting a .env", () => {
  it("reads the things a real .env contains", () => {
    const { vars, errors } = parseDotenv(
      [
        "# a comment",
        "",
        "export NODE_ENV=production",
        'API_KEY="quoted value"',
        "DATABASE_URL=postgres://u:p@h:5432/db?sslmode=require",
        "EMPTY=",
      ].join("\n")
    );
    expect(errors).toEqual([]);
    expect(vars).toEqual([
      { key: "NODE_ENV", value: "production" },
      { key: "API_KEY", value: "quoted value" },
      { key: "DATABASE_URL", value: "postgres://u:p@h:5432/db?sslmode=require" },
      { key: "EMPTY", value: "" },
    ]);
  });

  // A paste that silently loses three of forty variables is a container that
  // dies on start for a reason nothing in the UI mentioned.
  it("reports the lines it could not read instead of dropping them", () => {
    const { vars, errors } = parseDotenv("GOOD=1\njust some prose\n9BAD=2\n");
    expect(vars).toEqual([{ key: "GOOD", value: "1" }]);
    expect(errors.map((e) => e.line)).toEqual([2, 3]);
    expect(errors[1].reason).toContain("9BAD");
  });
});

describe("merging a paste into the seeded list", () => {
  // The whole point of pasting a .env over a seeded list is to fill in values
  // for keys detection already found; two rows named DATABASE_URL is a resource
  // whose environment depends on iteration order.
  it("fills in an existing key rather than duplicating it", () => {
    const seeded = seedEnvVars(["DATABASE_URL", "LOG_LEVEL"]);
    const merged = mergeEnvVars(seeded, [{ key: "DATABASE_URL", value: "postgres://x" }]);
    expect(merged.filter((v) => v.key === "DATABASE_URL")).toHaveLength(1);
    expect(merged.find((v) => v.key === "DATABASE_URL")?.value).toBe("postgres://x");
  });

  it("reuses the trailing blank row before appending", () => {
    const seeded = seedEnvVars(["A"]);
    const merged = mergeEnvVars(seeded, [{ key: "B", value: "2" }]);
    expect(merged.map((v) => v.key)).toEqual(["A", "B", ""]);
  });

  it("marks a pasted credential", () => {
    const merged = mergeEnvVars(seedEnvVars([]), [{ key: "STRIPE_SECRET", value: "sk_x" }]);
    expect(merged.find((v) => v.key === "STRIPE_SECRET")?.secret).toBe(true);
  });
});

describe("what gets created, and what blocks", () => {
  it("submits only rows with both a key and a value", () => {
    const drafts = [
      { id: "1", key: "A", value: "1", secret: false },
      { id: "2", key: "B", value: "", secret: false },
      { id: "3", key: "", value: "orphan", secret: false },
    ];
    expect(submittableEnvVars(drafts).map((d) => d.key)).toEqual(["A"]);
    expect(envVarCount(drafts)).toBe(1);
  });

  // The server rejects a malformed key, and by then the resource already
  // exists — which surfaces as a misleading "Create failed" (SIGMA-151).
  it("blocks the step on a malformed key, and tolerates an empty one", () => {
    expect(envVarsValid([{ id: "1", key: "9BAD", value: "x", secret: false }])).toBe(false);
    expect(envVarsValid([{ id: "1", key: "", value: "", secret: false }])).toBe(true);
    expect(envVarsValid([{ id: "1", key: "GOOD_KEY", value: "x", secret: false }])).toBe(true);
  });
});
