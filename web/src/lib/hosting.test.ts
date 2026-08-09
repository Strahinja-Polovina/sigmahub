import { readFileSync, readdirSync, statSync } from "node:fs";
import { createHash } from "node:crypto";
import { join, relative, sep } from "node:path";
import { describe, expect, it } from "vitest";
import {
  ALLOWED_SERVER_TYPES,
  CATALOG_SOURCE_SHA256,
  CLUSTER_EXCLUDED_KINDS,
  CONNECTABLE_SERVER_TYPES,
  DB_ENGINE_CATALOG,
  DB_ENGINE_KINDS,
  DEFAULT_S3_ENGINE,
  HOSTS_NOTHING_REASON,
  MESH_PORT_BASE,
  RESOURCE_KINDS,
  RESOURCE_KIND_LABELS,
  S3_ENGINE_CATALOG,
  S3_ENGINE_NAMES,
  SERVER_CATALOG,
  SERVER_TYPES,
  SERVER_TYPE_HOSTS,
  SERVER_TYPE_LABELS,
  SERVER_UNIT_WEIGHTS,
  canHost,
  categoryForKind,
  clusterCanHost,
  databaseConnectionUrl,
  isDatabaseEngine,
  isS3Engine,
  s3EndpointUrl,
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
// hashes them (store.CatalogSourceFiles). All six, because the output embeds
// more than the catalog: hashing only server_catalog.go let a currency change
// in billing.go ship a dashboard that still said EUR, with the whole web suite
// green. clusters.go joined the list when CLUSTER_EXCLUDED_KINDS started being
// rendered from it — an exclusion the web cannot see change is the same failure
// wearing a different hat, and demo mode reads that list with no control plane
// to correct it.
//
// db_engines.go and s3_engines.go joined when the engine table stopped being
// restated in demo-connection.ts. Every value in that copy disagreed with the
// control plane — postgres:17-alpine against a pin of 16.6, minio/minio:latest
// against an agent that refuses floating tags — and a version bump on the Go
// side is exactly the edit this list has to make visible here.
const CATALOG_SOURCES = [
  "server_catalog.go",
  "server_catalog_ts.go",
  "billing.go",
  "clusters.go",
  "db_engines.go",
  "s3_engines.go",
];

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
  //
  // It used to enforce it for ONE shape — the vocabulary written as object KEYS
  // — which meant it could not see the copies that were actually out there. A
  // four-name DB_KINDS list lived in two modules that never see each other, one
  // an array and one a Set, and a probe file carrying full array copies of BOTH
  // vocabularies (in the banned "mongo" spelling, no less) plus a
  // re-implemented canHost passed this suite 13/13. A guard with a blind spot
  // that wide reads as protection and is not (SIGMA-216). Four shapes now, one
  // per way this project has actually written a vocabulary down.
  //
  // Every shape is built from the same definition of a WORD, and that is the
  // whole reason the two false positives this guard has already paid for stay
  // out. `image: "postgres:16"` — a docker image tag in a demo compose fixture,
  // three of which once failed a guard about something else entirely — and
  // `mysql://…` — a connection string, three of which failed it again the day
  // the demo's URLs started being asserted in full — both carry a character no
  // member of either vocabulary contains. Neither is ever a word here, so no
  // widening below can bring either back.
  //
  // This file is inside src/, so the guard reads ITSELF. That is deliberate —
  // the test that forbids a copy is no more entitled to one than anything else
  // — and it is why the shapes below are described rather than illustrated:
  // spelling out an example list here would be writing the copy this test
  // exists to refuse.
  const WORD = /^(["'])([a-z0-9][a-z0-9-]*)\1$/;

  /** Shape 1: the vocabulary as object KEYS — `postgres: {…}`.
   *
   *  Bare or fully quoted, and never preceded by a quote: the looser
   *  `["']?name["']?\s*:` counted the colon INSIDE a string literal, which is
   *  how an image tag read as a table keyed by resource kind. The `//`
   *  lookahead is the same refusal for a URL scheme — nothing that is really a
   *  key is followed by two slashes. */
  function keyEnumeration(vocabulary: readonly string[], src: string): string[] {
    const named = vocabulary.filter((v) =>
      new RegExp(`(^|[^\\w\\-"'\`])(${v}|"${v}"|'${v}')\\s*:(?!//)`, "m").test(src)
    );
    return named.length >= 3 ? [`as keys: ${named.join(", ")}`] : [];
  }

  /** Shape 2: the vocabulary as an ARRAY of quoted words.
   *
   *  The shape both DB_KINDS copies were written in, and the one the key
   *  matcher was structurally unable to see. Only FLAT literals whose every
   *  element is a quoted word count, which is what keeps demo fixtures out: an
   *  array of resource ROWS is an array of objects, so it never gets here.
   *
   *  A word the vocabulary does NOT contain does not disqualify the literal —
   *  only a punctuation-carrying one does. Otherwise a copy that misspells
   *  "mongodb" as "mongo" would be the single copy this test waves through, and
   *  that spelling is the exact bug the dashboard already shipped once.
   *
   *  Three words is the line, the same one shape 1 draws, and it applies to a
   *  fixture as much as to a table: a test that wants a project holding three
   *  kinds of host says so from the catalog rather than by typing three names,
   *  which is both shorter to read and true after the eighth type lands. */
  function arrayEnumeration(vocabulary: readonly string[], src: string): string[] {
    const found: string[] = [];
    for (const [literal] of src.matchAll(/\[[^[\]{}]*\]/g)) {
      const parts = literal
        .slice(1, -1)
        .split(",")
        .map((p) => p.trim())
        .filter((p) => p !== "");
      if (parts.length === 0) continue;
      const words = parts.map((p) => WORD.exec(p)?.[2]);
      if (words.some((w) => w === undefined)) continue;
      const named = [...new Set(words as string[])].filter((w) => vocabulary.includes(w));
      if (named.length >= 3) found.push(`as an array: ${named.join(", ")}`);
    }
    return found;
  }

  /** Shape 3: the vocabulary in VALUE position — `{ kind: "postgres" }`.
   *
   *  How a table gets written when the word is the entry's identity rather than
   *  its key, and invisible to shape 1 for exactly that reason.
   *
   *  Demo FIXTURES put kinds in value position too — every mock resource row
   *  carries one — so repetition is what separates the two: an enumeration
   *  names each word once and stops, while data says "app" on four rows and
   *  "postgres" on two. A file where any word repeats there is describing
   *  resources, not the set of kinds, and is left alone. */
  function valueEnumeration(vocabulary: readonly string[], src: string): string[] {
    const counts = new Map<string, number>();
    for (const word of vocabulary) {
      const key = `(?:[A-Za-z_$][\\w$]*|"[^"\\n]*"|'[^'\\n]*')`;
      const re = new RegExp(`(?:^|[{,])\\s*${key}\\s*:\\s*(["'])${word}\\1`, "gm");
      const hits = [...src.matchAll(re)].length;
      if (hits > 0) counts.set(word, hits);
    }
    if (counts.size < 3) return [];
    if ([...counts.values()].some((n) => n > 1)) return [];
    return [`in value position: ${[...counts.keys()].join(", ")}`];
  }

  /** Shape 4: the vocabulary as PROSE — a `|`-separated run, as a column
   *  comment or a TypeScript union writes one.
   *
   *  The copy that survived longest and the smallest one there is, because a
   *  comment is the only form of this that no generator can write and no
   *  reviewer diffs. Both columns in the schema documented their vocabulary
   *  this way, and the servers.type one had been four names long since before
   *  the product grew a VPS, a cluster node and a build server: the file that
   *  defines the column described a fleet three types smaller than the one it
   *  stores, and nothing anywhere could notice.
   *
   *  Runs of three or more words joined by a SINGLE `|`, so `!gpu || !gpu.count`
   *  is not a run, and a run naming no member of this vocabulary — the
   *  neighbouring `queued | building | running | success | failed` — is not a
   *  copy of it. */
  function proseEnumeration(vocabulary: readonly string[], src: string): string[] {
    const token = `["']?[A-Za-z0-9][\\w-]*["']?`;
    const runs = src.matchAll(new RegExp(`(?<![|\\w])${token}(?:\\s*\\|(?!\\|)\\s*${token}){2,}`, "g"));
    const found: string[] = [];
    for (const [run] of runs) {
      const words = run.split("|").map((w) => w.trim().replace(/^["']|["']$/g, ""));
      const named = [...new Set(words)].filter((w) => vocabulary.includes(w));
      if (named.length >= 3) found.push(`as prose: ${named.join(", ")}`);
    }
    return found;
  }

  function vocabularyCopies(vocabulary: readonly string[], typeName: string) {
    const offenders: string[] = [];
    for (const file of files) {
      const src = readFileSync(file, "utf8");
      const name = relative(process.cwd(), file);
      // The Record escape belongs to shape 1 alone: a table keyed on the type
      // IS the sanctioned way to hold one, and tsc keeps it exhaustive. It
      // excuses nothing about an array, a value list or a comment, none of
      // which tsc checks at all.
      const keyed = new RegExp(`Record<\\s*${typeName}\\s*,`).test(src);
      const found = [
        ...(keyed ? [] : keyEnumeration(vocabulary, src)),
        ...arrayEnumeration(vocabulary, src),
        ...valueEnumeration(vocabulary, src),
        ...proseEnumeration(vocabulary, src),
      ];
      for (const hit of found) offenders.push(`${name} (${hit})`);
    }
    return offenders;
  }

  it("finds no untyped copy of the server-type list", () => {
    // Five modules used to hold this list, and the one that mattered most — the
    // CP's own HTTP boundary — was the one nobody remembered to update.
    expect(
      vocabularyCopies(SERVER_TYPES, "ServerType"),
      "import SERVER_TYPES from @/lib/server-catalog.generated, or key the table on ServerType so tsc keeps it exhaustive — a list written out here agrees with the control plane only by luck"
    ).toEqual([]);
  });

  it("finds no untyped copy of the resource-kind list", () => {
    expect(
      vocabularyCopies(RESOURCE_KINDS, "ResourceKind"),
      "import RESOURCE_KINDS from @/lib/server-catalog.generated, or key the table on ResourceKind so tsc keeps it exhaustive — a list written out here agrees with the control plane only by luck"
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
    // Every engine the control plane provisions, not the four that existed when
    // this rule was written: a fifth added to the Go catalog is exactly the one
    // nobody would think to add here.
    for (const kind of DB_ENGINE_KINDS) {
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

// The other half of "where can this run". It arrives here compiled in rather
// than over the wire because demo mode has no control plane to ask, and the
// empty list it used to hard-code made clusterEligible() answer true for every
// kind — a demo cluster offered as a target for a Postgres the real product
// refuses.
describe("the kinds a cluster refuses", () => {
  it("names only kinds this dashboard knows about", () => {
    // A typo here would be a kind excluded in the control plane and offered
    // here, which is the disagreement the generated list exists to end.
    for (const kind of CLUSTER_EXCLUDED_KINDS) {
      expect(RESOURCE_KINDS).toContain(kind);
    }
  });

  it("answers clusterCanHost for every kind, and for a string that is not one", () => {
    for (const kind of RESOURCE_KINDS) {
      expect(clusterCanHost(kind)).toBe(!CLUSTER_EXCLUDED_KINDS.includes(kind));
    }
    // A deny list, exactly as the control plane reads it: anything unlisted is
    // allowed, and the create call is what rejects a kind nobody defined.
    expect(clusterCanHost("not-a-kind")).toBe(true);
  });

  it("keeps stateful engines and model endpoints out of the cluster", () => {
    // Each has its own reason and both are product decisions: an engine
    // rescheduled onto a node without its volume is data loss, and nothing
    // renders a cluster-targeted model endpoint at all.
    // Stated as "every engine, plus storage, plus models" rather than as
    // CLUSTER_EXCLUDED_KINDS, which would assert the list against itself and
    // pass whatever it said. Naming the two non-engine kinds is the product
    // decision; the engines come from the catalog so a new one is refused by
    // the cluster from the day it exists.
    for (const kind of [...DB_ENGINE_KINDS, "s3", "llm"] as ResourceKind[]) {
      expect(clusterCanHost(kind)).toBe(false);
    }
    expect(clusterCanHost("app")).toBe(true);
  });
});

// What a managed engine IS — the image, the connection-URL shape and the port
// range — arrives here compiled in for the same reason the cluster exclusions
// do: demo mode has no control plane to ask, and it answered from a table of
// its own where postgres:17-alpine stood against a control plane pinned to
// 16.6. Both panels print the value verbatim under a label reading "Engine", so
// the copy was not a stale detail, it described a different product.
describe("the engine catalog is the control plane's", () => {
  it("pins every image to a version tag or a digest, never latest", () => {
    // The agent's own policy: "the floating 'latest' tag is not permitted; pin
    // a version tag or digest" (agent/internal/container/policy.go). The demo
    // advertised minio/minio:latest and chrislusf/seaweedfs:latest — images
    // this product would have refused to run.
    const images = [
      ...DB_ENGINE_KINDS.map((kind) => DB_ENGINE_CATALOG[kind].image),
      ...S3_ENGINE_NAMES.map((engine) => S3_ENGINE_CATALOG[engine].image),
    ];
    expect(images.length).toBeGreaterThan(0);
    for (const image of images) {
      if (image.includes("@sha256:")) continue; // digest-pinned: immutable
      const tag = image.slice(image.lastIndexOf(":") + 1);
      expect(tag, `${image} carries no version tag`).not.toBe(image);
      expect(tag, `${image} carries a tag the agent policy refuses`).not.toBe("latest");
    }
  });

  it("describes every database kind, and only database kinds", () => {
    for (const kind of DB_ENGINE_KINDS) {
      expect(RESOURCE_KINDS).toContain(kind);
      expect(categoryForKind(kind)).toBe("database");
      expect(DB_ENGINE_CATALOG[kind].engine).toBe(kind);
    }
    for (const kind of RESOURCE_KINDS) {
      expect(isDatabaseEngine(kind)).toBe(categoryForKind(kind) === "database");
    }
  });

  it("renders each engine's URL from its own template, filling every placeholder", () => {
    for (const kind of DB_ENGINE_KINDS) {
      const url = databaseConnectionUrl(kind, {
        username: "sigma",
        password: "s3cret",
        host: "10.8.0.21",
        port: MESH_PORT_BASE,
        database: "orders",
      });
      // A leftover {placeholder} is a template the renderer does not know how
      // to fill — a connection string nobody can paste anywhere.
      expect(url, kind).not.toMatch(/[{}]/);
      expect(url, kind).toContain(`10.8.0.21:${MESH_PORT_BASE}`);
      expect(url, kind).toContain("s3cret");
    }
  });

  it("fills a template once, so a credential is never re-read as a placeholder", () => {
    // Single-pass substitution is what makes the Go and TypeScript renderers
    // one renderer; a second pass would also let a password rewrite the host.
    const url = databaseConnectionUrl("postgres", {
      username: "sigma",
      password: "{host}",
      host: "10.8.0.21",
      port: MESH_PORT_BASE,
      database: "orders",
    });
    expect(url).toBe(`postgresql://sigma:{host}@10.8.0.21:${MESH_PORT_BASE}/orders`);
  });

  it("has no endpoint for a host that has not finished mesh enrollment", () => {
    // The control plane answers with an empty endpoint until the server has a
    // mesh IP; a URL pointing at no address would be worse than none.
    expect(s3EndpointUrl(DEFAULT_S3_ENGINE, "", MESH_PORT_BASE)).toBe("");
    expect(s3EndpointUrl(DEFAULT_S3_ENGINE, "10.8.0.9", MESH_PORT_BASE)).toBe(
      `http://10.8.0.9:${MESH_PORT_BASE}`
    );
  });

  it("names a mesh port base that is none of the engines' own ports", () => {
    // The allocator's range starts here, and the numbers it must not start on
    // are the ports the engines listen on inside their containers — 5432, 3306,
    // 27017, 6379 for the databases, 9000 and 8333 for the object stores. A
    // panel printing one of those is printing a port nothing outside the
    // container ever answers on, which is what demo mode used to do.
    for (const containerPort of [5432, 3306, 27017, 6379, 9000, 8333]) {
      expect(MESH_PORT_BASE).not.toBe(containerPort);
    }
    expect(MESH_PORT_BASE).toBeGreaterThan(1024);
  });

  it("knows the default object-storage engine, and it is a real one", () => {
    expect(isS3Engine(DEFAULT_S3_ENGINE)).toBe(true);
    expect(S3_ENGINE_NAMES).toContain(DEFAULT_S3_ENGINE);
    expect(isS3Engine("ceph")).toBe(false);
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
