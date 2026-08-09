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
 *
 * What this module does NOT decide is what an engine IS. The image, the
 * connection-URL shape and the port range all belong to the control plane, and
 * this file used to restate all three from memory: postgres:17-alpine for a CP
 * pinned to 16.6, a Postgres URL carrying ?sslmode=disable the CP never
 * renders, a MongoDB URL with the database in the path the CP leaves out, and
 * minio/minio:latest — an image the agent's own policy refuses to run. They are
 * read from the generated catalog now (@/lib/server-catalog.generated), so the
 * demo describes THIS product or fails to build.
 */

import {
  DB_ENGINE_CATALOG,
  DEFAULT_S3_ENGINE,
  MESH_PORT_BASE,
  S3_ENGINE_CATALOG,
  databaseConnectionUrl,
  isDatabaseEngine,
  s3EndpointUrl,
  type DatabaseEngine,
  type S3Engine,
} from "@/lib/server-catalog.generated";

/** Whether this kind is provisioned as a managed database engine. Takes a
 *  plain string: a resource row carries one, and a kind the catalog does not
 *  know is simply not a database.
 *
 *  It is the control plane's own answer (the engine table is the catalog's), so
 *  a fifth engine added there needs no edit here — the table that used to live
 *  in this file listed the four kinds by hand and would have answered "not a
 *  database" for the new one, leaving its panel blank. */
export function isDemoDatabaseKind(kind: string): boolean {
  return isDatabaseEngine(kind);
}

/** djb2 over a string. One arithmetic for every derived value, so two demo
 *  values derived from the same id cannot disagree about what "derived from the
 *  id" means — the seed and the simulated check-in use it too. */
function hash(input: string): number {
  let h = 5381;
  for (const c of input) h = ((h << 5) + h + c.charCodeAt(0)) >>> 0;
  return h;
}

/** A stable token derived from a string. */
function token(input: string, length: number): string {
  let out = "";
  let x = hash(input);
  const alphabet = "abcdefghijkmnpqrstuvwxyz23456789";
  for (let i = 0; i < length; i++) {
    out += alphabet[x % alphabet.length];
    // Re-mix rather than shift: a plain shift runs out of entropy after six
    // characters and every id ends with the same tail.
    x = (x * 33 + i + 7) >>> 0;
  }
  return out;
}

/** How far above the base a demo port may land. Wide enough that two engines
 *  on one demo host practically never collide, narrow enough that the number
 *  still reads like an allocation rather than a random high port. */
const MESH_PORT_SPAN = 1000;

/**
 * The mesh port this resource would have been allocated.
 *
 * The port is NOT a property of the engine. The control plane hands out the
 * next free number per server from MESH_PORT_BASE (store.allocateDBPort), and
 * the engine's container port sits behind it, dialled by nobody. This panel
 * used to print that container port, so the demo taught "your Postgres is on
 * 5432" about a product that answers on 15000+ — a number an operator would
 * have written down and then had to learn again.
 *
 * It is DERIVED from the resource id rather than allocated, like everything
 * else in this file. Reproducing the real number would mean reproducing the
 * create-and-delete history of every port-owning resource on that host, which
 * demo mode keeps no record of; and deriving buys the property the panel
 * actually needs, which is that the wizard's port and the resource page's port
 * a week later are the same number. Ranking a resource among its siblings would
 * have renumbered the whole server every time another resource was created.
 */
function meshPort(resourceId: string): number {
  return MESH_PORT_BASE + (hash(`${resourceId}:port`) % MESH_PORT_SPAN);
}

export type DemoDatabaseInfo = {
  resourceId: string;
  engine: DatabaseEngine;
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
  if (!isDatabaseEngine(input.kind)) return null;
  const spec = DB_ENGINE_CATALOG[input.kind];
  return {
    resourceId: input.resourceId,
    engine: spec.engine,
    image: spec.image,
    host: (input.meshIp ?? "").trim() || `${input.resourceName}.sigma.internal`,
    port: meshPort(input.resourceId),
    database: input.resourceName.replace(/-/g, "_"),
    username: `sigma_${token(`${input.resourceId}:user`, 6)}`,
    // Always. It is the product's rule, not a per-resource setting, and the
    // panel renders a lock badge off it.
    meshOnly: true,
  };
}

/** The secret half, behind the same Project Admin gate the control plane
 *  applies. `demo_` is part of the value and not a label beside it: it travels
 *  with the string into whatever the reader pastes it into.
 *
 *  The URL is rendered from the engine's own template, by the same function
 *  name the control plane's ConnectionURL fills — this file no longer knows
 *  which engines take a database in the path, which one authenticates on
 *  admin, or which one takes no username at all. */
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
    url: databaseConnectionUrl(info.engine, {
      username: info.username,
      password,
      host: info.host,
      port: info.port,
      database: info.database,
    }),
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

export function demoS3Info(input: {
  resourceId: string;
  resourceName: string;
  engine?: string;
  meshIp?: string | null;
}): DemoS3Info {
  const engine = (input.engine ?? "").trim() || DEFAULT_S3_ENGINE;
  // An engine the catalog does not know still gets an endpoint, described with
  // the default engine's image and shape: the resource exists either way, and a
  // blank panel would be a worse answer than a described one. Its own name is
  // kept, because that is what the resource says it is.
  const spec = S3_ENGINE_CATALOG[engine as S3Engine] ?? S3_ENGINE_CATALOG[DEFAULT_S3_ENGINE];
  const host = (input.meshIp ?? "").trim() || `${input.resourceName}.sigma.internal`;
  // The same allocated mesh port a database gets, for the same reason: the S3
  // API listens on 9000 (MinIO) or 8333 (SeaweedFS) inside the container, and
  // the endpoint an operator dials is the allocated one. The demo printed 9000
  // for both engines, which was the container port of one of them.
  const port = meshPort(input.resourceId);
  return {
    resourceId: input.resourceId,
    engine,
    image: spec.image,
    // Uppercase because every S3 client's examples are, and an operator
    // comparing this against their own AWS config should not have to wonder
    // whether the case matters.
    accessKey: token(`${input.resourceId}:access`, 20).toUpperCase(),
    host,
    port,
    meshOnly: true,
    endpoint: s3EndpointUrl(spec.engine, host, port),
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
