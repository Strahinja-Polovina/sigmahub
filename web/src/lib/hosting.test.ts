import { readFileSync, readdirSync, statSync } from "node:fs";
import { createHash } from "node:crypto";
import { join, relative, sep } from "node:path";
import { describe, expect, it } from "vitest";
import {
  ALLOWED_SERVER_TYPES,
  CATALOG_SOURCE_SHA256,
  CONNECTABLE_SERVER_TYPES,
  HOSTS_NOTHING_REASON,
  RESOURCE_KINDS,
  RESOURCE_KIND_LABELS,
  SERVER_CATALOG,
  SERVER_TYPES,
  SERVER_TYPE_HOSTS,
  SERVER_TYPE_LABELS,
  SERVER_UNIT_WEIGHTS,
  canHost,
} from "./server-catalog.generated";
import type { ResourceKind, ServerType } from "./server-catalog.generated";

// This file is the web half of SIGMA-198's parity guard.
//
// It used to reconcile a HAND-MAINTAINED matrix in src/lib/hosting.ts against a
// regex parse of the control plane's Go source. That had two holes big enough
// to ship the bug it was written to prevent: it only asserted web ⊇ CP (a
// web-only phantom type passed), and a regex that matched a SUBSET of a Go map
// literal — after a gofmt run, or a comment inside the literal — passed while
// checking almost nothing.
//
// The matrix is now GENERATED from the CP catalog, so there is nothing to
// reconcile. What is left to protect is the mechanism itself:
//
//   1. the generated module really was rendered from the Go source on disk
//      (a CP edit that skipped `go generate` must fail HERE, not in production);
//   2. no second copy of the vocabulary grows back anywhere in src/;
//   3. the domain rules the matrix encodes still hold.

const REPO = join(process.cwd(), "..");
const STORE_GO = join(REPO, "cp", "internal", "store");
const GENERATED = join("src", "lib", "server-catalog.generated.ts");

// Every input the generated module is rendered from, in the order the Go side
// hashes them (store.CatalogSourceFiles). All three, because the output embeds
// more than the catalog: hashing only server_catalog.go let a currency change
// in billing.go ship a dashboard that still said EUR, with the whole web suite
// green.
const CATALOG_SOURCES = ["server_catalog.go", "server_catalog_ts.go", "billing.go"];

describe("the generated catalog tracks the control plane", () => {
  it("was rendered from the Go catalog currently on disk", () => {
    // Hashing beats parsing: it cannot silently match a subset, and it fails on
    // ANY divergence rather than only on the parts a regex happened to cover.
    // The framing (name:length header per file) mirrors CatalogSourceDigest, so
    // moving a byte across a file boundary cannot produce the same digest.
    const h = createHash("sha256");
    for (const name of CATALOG_SOURCES) {
      const src = readFileSync(join(STORE_GO, name));
      h.update(`${name}:${src.length}\n`);
      h.update(src);
    }
    const sha = h.digest("hex");
    expect(
      sha,
      `${GENERATED} is stale — run \`cd cp && go generate ./...\` and commit the result`
    ).toBe(CATALOG_SOURCE_SHA256);
  });

  it("carries every server type with a label, a hint and requirements", () => {
    expect(SERVER_TYPES.length).toBeGreaterThan(0);
    for (const type of SERVER_TYPES) {
      const spec = SERVER_CATALOG[type];
      expect(spec.type).toBe(type);
      expect(spec.label).toBeTruthy();
      expect(spec.hint).toBeTruthy();
      expect(SERVER_TYPE_LABELS[type]).toBe(spec.label);
      expect(SERVER_UNIT_WEIGHTS[type]).toBe(spec.unitWeight);
      // Requirements are what the connect dialog promises and what SIGMA-203
      // will enforce; a type with none would silently accept any host.
      expect(spec.requires.distros.length).toBeGreaterThan(0);
      expect(spec.requires.arches.length).toBeGreaterThan(0);
      expect(spec.requires.checks.length).toBeGreaterThan(0);
      for (const check of spec.requires.checks) {
        expect(check.text).toBeTruthy();
        expect(check.fact).toBeTruthy();
      }
    }
  });

  it("keeps both directions of the matrix in agreement", () => {
    for (const type of SERVER_TYPES) {
      for (const kind of SERVER_TYPE_HOSTS[type]) {
        expect(ALLOWED_SERVER_TYPES[kind]).toContain(type);
      }
    }
    for (const kind of RESOURCE_KINDS) {
      for (const type of ALLOWED_SERVER_TYPES[kind]) {
        expect(canHost(type, kind)).toBe(true);
      }
    }
  });
});

// Walk src/, skipping the generated module itself.
function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) {
      sourceFiles(path, out);
    } else if (/\.tsx?$/.test(entry)) {
      out.push(path);
    }
  }
  return out;
}

describe("nothing keeps a second copy of the vocabulary", () => {
  const files = sourceFiles(join(process.cwd(), "src")).filter(
    (f) => relative(process.cwd(), f) !== GENERATED.split("/").join(sep)
  );

  // A module may still hold a per-type or per-kind TABLE — the demo-mode host
  // capacities are a legitimate one. What it may not do is enumerate the
  // vocabulary UNTYPED: `Record<ServerType, …>` is checked for exhaustiveness by
  // tsc, so adding a type to the CP catalog breaks the build at the omission,
  // while `Record<string, …>` silently keeps a stale list. That distinction is
  // exactly what these two tests enforce.
  function untypedEnumerations(vocabulary: readonly string[], typeName: string) {
    const offenders: string[] = [];
    for (const file of files) {
      const src = readFileSync(file, "utf8");
      const named = vocabulary.filter((v) =>
        new RegExp(`(^|[^\\w-])["']?${v}["']?\\s*:`, "m").test(src)
      );
      if (named.length < 3) continue;
      if (new RegExp(`Record<\\s*${typeName}\\s*,`).test(src)) continue;
      offenders.push(`${relative(process.cwd(), file)} (${named.join(", ")})`);
    }
    return offenders;
  }

  it("finds no untyped copy of the server-type list", () => {
    // Five modules used to hold this list, and the one that mattered most — the
    // CP's own HTTP boundary — was the one nobody remembered to update.
    expect(
      untypedEnumerations(SERVER_TYPES, "ServerType"),
      "import from @/lib/server-catalog.generated, or key the table on ServerType so tsc keeps it exhaustive"
    ).toEqual([]);
  });

  it("finds no untyped copy of the resource-kind list", () => {
    expect(
      untypedEnumerations(RESOURCE_KINDS, "ResourceKind"),
      "import from @/lib/server-catalog.generated, or key the table on ResourceKind so tsc keeps it exhaustive"
    ).toEqual([]);
  });

  it("has no module left importing the deleted hand-written copies", () => {
    for (const file of files) {
      const src = readFileSync(file, "utf8");
      expect(src, relative(process.cwd(), file)).not.toMatch(/from "[^"]*\/hosting"/);
      expect(src, relative(process.cwd(), file)).not.toMatch(/from "[^"]*server-meta"/);
    }
  });
});

// The rules the matrix exists to express. Each was a product decision, and each
// is one word away from being reversed by accident.
describe("availability matrix", () => {
  it("never lets a database land on a storage or gpu server", () => {
    for (const kind of ["postgres", "mysql", "mongodb", "redis"] as ResourceKind[]) {
      expect(canHost("storage", kind)).toBe(false);
      expect(canHost("gpu", kind)).toBe(false);
    }
  });

  it("treats a VPS as a general-purpose host", () => {
    // Virtualization is a disclosure, not a capability difference.
    expect(SERVER_TYPE_HOSTS.vps).toEqual(SERVER_TYPE_HOSTS.general);
  });

  it("schedules nothing directly onto cluster or build nodes", () => {
    for (const type of ["k8s", "build"] as ServerType[]) {
      expect(SERVER_TYPE_HOSTS[type]).toEqual([]);
      // And says why, so the UI never shows an unexplained empty list.
      expect(HOSTS_NOTHING_REASON[type]).toBeTruthy();
    }
  });

  it("only serves models on GPU hardware", () => {
    for (const type of SERVER_TYPES) {
      expect(canHost(type, "llm")).toBe(type === "gpu");
    }
  });

  it("does not offer a cluster node when connecting a new server", () => {
    // A node becomes one by JOINING a cluster. Offering it at enrollment makes
    // a host nothing can be scheduled onto that still bills at cluster weight.
    expect(CONNECTABLE_SERVER_TYPES).not.toContain("k8s");
    // But it stays a known type: the API accepts every canonical type, and a
    // narrower list at the edge is the bug SIGMA-198 removed.
    expect(SERVER_TYPES).toContain("k8s");
  });
});

describe("the kind vocabulary is the control plane's", () => {
  it("spells MongoDB the way the CP, the agent and every migration do", () => {
    // The dashboard used to say "mongo" and needed two opposite-facing
    // translators plus three dual-spelling call sites to hide the difference.
    expect(RESOURCE_KINDS).toContain("mongodb");
    expect(RESOURCE_KINDS).not.toContain("mongo");
    expect(RESOURCE_KIND_LABELS.mongodb).toBe("MongoDB");
  });

  it("labels every kind", () => {
    for (const kind of RESOURCE_KINDS) {
      expect(RESOURCE_KIND_LABELS[kind]).toBeTruthy();
    }
  });
});
