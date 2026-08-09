/**
 * The connection details a managed resource reports in demo mode (SIGMA-215).
 *
 * A managed engine's credentials are GENERATED — that is the whole shape of the
 * flow: the wizard asks for a version and a server, the engine comes up, and
 * what comes back is a user, a password and a URL, shown once and then behind
 * an audited reveal. With no control plane there was no engine to generate
 * anything, so `getDatabaseInfo` and `revealDatabaseConnection` threw, the two
 * panels that render them never mounted, and the last screen of the wizard was
 * blank for five of the seven resource kinds. Someone evaluating the product
 * offline could create a Postgres and never find out that it hands them a
 * connection string.
 *
 * Every value here is derived from the resource's own id, so it is stable: the
 * URL on the resource page a week later is the one the wizard showed. And every
 * SECRET is prefixed `demo_`, deliberately and visibly — a plausible-looking
 * password for a database that does not exist is the one thing on this screen
 * that could be mistaken for something to write down.
 *
 * The host is the mesh address, because meshOnly is the fact this panel exists
 * to state: a managed engine is never published on a public interface, and a
 * demo that showed a routable host would teach the opposite of the product's
 * most load-bearing default.
 */

import type { ResourceKind } from "@/lib/server-catalog.generated";

/**
 * The engine, image and port each database kind is provisioned as. Ports are
 * the engines' own defaults — the wizard's networking step is skipped for a
 * managed kind precisely because there is nothing to choose here.
 *
 * Keyed on the generated ResourceKind and therefore EXHAUSTIVE, with the three
 * kinds that are not managed databases written as an explicit null rather than
 * left out. A `Record<string, …>` would let a resource kind added to the
 * control plane's catalog quietly fall through to "not a database", which is
 * the class of stale second copy hosting.test.ts exists to catch.
 */
type DatabaseEngineSpec = { engine: string; image: string; port: number };

const DATABASE_ENGINES: Record<ResourceKind, DatabaseEngineSpec | null> = {
  postgres: { engine: "postgres", image: "postgres:17-alpine", port: 5432 },
  mysql: { engine: "mysql", image: "mysql:8.4", port: 3306 },
  mongodb: { engine: "mongodb", image: "mongo:8", port: 27017 },
  redis: { engine: "redis", image: "redis:7-alpine", port: 6379 },
  // Not managed engines: an application brings its own image, object storage
  // has its own panel below, and a model endpoint's "connection" is an HTTP
  // API rather than a database URL.
  app: null,
  s3: null,
  llm: null,
};

const S3_IMAGES: Record<string, string> = {
  minio: "minio/minio:latest",
  seaweedfs: "chrislusf/seaweedfs:latest",
};

/** Whether this kind is provisioned as a managed database engine. Takes a
 *  plain string: a resource row carries one, and a kind the catalog does not
 *  know is simply not a database. */
export function isDemoDatabaseKind(kind: string): boolean {
  return DATABASE_ENGINES[kind as ResourceKind] != null;
}

/** A stable token derived from a string. djb2, the same one the seed and the
 *  simulated check-in use — one arithmetic, so two demo values derived from the
 *  same id cannot disagree about what "derived from the id" means. */
function token(input: string, length: number): string {
  let h = 5381;
  for (const c of input) h = ((h << 5) + h + c.charCodeAt(0)) >>> 0;
  let out = "";
  let x = h;
  const alphabet = "abcdefghijkmnpqrstuvwxyz23456789";
  for (let i = 0; i < length; i++) {
    out += alphabet[x % alphabet.length];
    // Re-mix rather than shift: a plain shift runs out of entropy after six
    // characters and every id ends with the same tail.
    x = (x * 33 + i + 7) >>> 0;
  }
  return out;
}

export type DemoDatabaseInfo = {
  resourceId: string;
  engine: string;
  image: string;
  host: string;
  port: number;
  database: string;
  username: string;
  meshOnly: boolean;
};

export type DemoDatabaseConnection = DemoDatabaseInfo & {
  password: string;
  url: string;
};

/** The scheme each engine's connection URL uses. Redis has no database name in
 *  the sense the others do, and a URL that invented one would not connect. */
function connectionUrl(
  engine: string,
  info: { host: string; port: number; database: string; username: string },
  password: string
): string {
  const auth = `${encodeURIComponent(info.username)}:${encodeURIComponent(password)}`;
  switch (engine) {
    case "postgres":
      return `postgresql://${auth}@${info.host}:${info.port}/${info.database}?sslmode=disable`;
    case "mysql":
      return `mysql://${auth}@${info.host}:${info.port}/${info.database}`;
    case "mongodb":
      return `mongodb://${auth}@${info.host}:${info.port}/${info.database}?authSource=admin`;
    default:
      // Redis authenticates with the password alone; a username in the URL is
      // accepted by redis 6+ ACLs and ignored by everything older, so the demo
      // prints the form that works against both.
      return `redis://:${encodeURIComponent(password)}@${info.host}:${info.port}/0`;
  }
}

/**
 * The non-secret half — what any member may see.
 *
 * `host` falls back to the resource id's own service name when the demo server
 * has no mesh address yet (a host that has not checked in). That is not a
 * placeholder: it is the name the container would be reachable under on the
 * mesh network, and it is what the operator would use before the tunnel is up.
 */
export function demoDatabaseInfo(input: {
  resourceId: string;
  resourceName: string;
  kind: string;
  meshIp?: string | null;
}): DemoDatabaseInfo | null {
  const spec = DATABASE_ENGINES[input.kind as ResourceKind];
  if (!spec) return null;
  return {
    resourceId: input.resourceId,
    engine: spec.engine,
    image: spec.image,
    host: (input.meshIp ?? "").trim() || `${input.resourceName}.sigma.internal`,
    port: spec.port,
    database: input.resourceName.replace(/-/g, "_"),
    username: `sigma_${token(`${input.resourceId}:user`, 6)}`,
    // Always. It is the product's rule, not a per-resource setting, and the
    // panel renders a lock badge off it.
    meshOnly: true,
  };
}

/** The secret half, behind the same Project Admin gate the control plane
 *  applies. `demo_` is part of the value and not a label beside it: it travels
 *  with the string into whatever the reader pastes it into. */
export function demoDatabaseConnection(input: {
  resourceId: string;
  resourceName: string;
  kind: string;
  meshIp?: string | null;
}): DemoDatabaseConnection | null {
  const info = demoDatabaseInfo(input);
  if (!info) return null;
  const password = `demo_${token(`${input.resourceId}:password`, 16)}`;
  return {
    ...info,
    password,
    url: connectionUrl(info.engine, info, password),
  };
}

export type DemoS3Info = {
  resourceId: string;
  engine: string;
  image: string;
  accessKey: string;
  host: string;
  port: number;
  meshOnly: boolean;
  endpoint: string;
};

export type DemoS3Connection = DemoS3Info & { secretKey: string };

/** The S3 port every engine in the catalog serves on. MinIO's console is a
 *  second port the product does not publish, so it is not shown. */
const S3_PORT = 9000;

export function demoS3Info(input: {
  resourceId: string;
  resourceName: string;
  engine?: string;
  meshIp?: string | null;
}): DemoS3Info {
  const engine = (input.engine ?? "minio").trim() || "minio";
  const host = (input.meshIp ?? "").trim() || `${input.resourceName}.sigma.internal`;
  return {
    resourceId: input.resourceId,
    engine,
    image: S3_IMAGES[engine] ?? S3_IMAGES.minio,
    // Uppercase because every S3 client's examples are, and an operator
    // comparing this against their own AWS config should not have to wonder
    // whether the case matters.
    accessKey: token(`${input.resourceId}:access`, 20).toUpperCase(),
    host,
    port: S3_PORT,
    meshOnly: true,
    endpoint: `http://${host}:${S3_PORT}`,
  };
}

export function demoS3Connection(input: {
  resourceId: string;
  resourceName: string;
  engine?: string;
  meshIp?: string | null;
}): DemoS3Connection {
  return {
    ...demoS3Info(input),
    secretKey: `demo_${token(`${input.resourceId}:secret`, 24)}`,
  };
}
