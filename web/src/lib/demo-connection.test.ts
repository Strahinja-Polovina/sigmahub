import { describe, expect, it } from "vitest";

import {
  demoDatabaseConnection,
  demoDatabaseInfo,
  demoS3Connection,
  demoS3Info,
  isDemoDatabaseKind,
} from "./demo-connection";
import {
  DB_ENGINE_CATALOG,
  DB_ENGINE_KINDS,
  MESH_PORT_BASE,
  RESOURCE_KINDS,
  S3_ENGINE_CATALOG,
} from "./server-catalog.generated";

const RES = { resourceId: "res_a1b2c3d4", resourceName: "orders-db", meshIp: "10.8.0.21" };

describe("which kinds are managed database engines", () => {
  it("answers for every kind in the catalog, with no fall-through", () => {
    const managed = RESOURCE_KINDS.filter((k) => isDemoDatabaseKind(k));
    expect(managed.sort()).toEqual([...DB_ENGINE_KINDS].sort());
  });

  it("is not a database for a kind the catalog has never heard of", () => {
    expect(isDemoDatabaseKind("cassandra")).toBe(false);
  });
});

describe("a demo database's connection details", () => {
  it("is nothing at all for a kind that is not a managed engine", () => {
    expect(demoDatabaseInfo({ ...RES, kind: "app" })).toBeNull();
    expect(demoDatabaseConnection({ ...RES, kind: "llm" })).toBeNull();
  });

  // The panel's whole claim: the engine is on the mesh and nowhere else. A demo
  // that printed a routable host would teach the opposite of the real default.
  it("is reachable on the host's MESH address, and says it is mesh-only", () => {
    const info = demoDatabaseInfo({ ...RES, kind: "postgres" })!;
    expect(info.host).toBe("10.8.0.21");
    expect(info.meshOnly).toBe(true);
  });

  // Not a placeholder: it is the name the container answers to on the mesh
  // before the tunnel is up, which is what an operator would actually type.
  it("falls back to the resource's mesh name when the host has no address yet", () => {
    const info = demoDatabaseInfo({ ...RES, meshIp: "", kind: "redis" })!;
    expect(info.host).toBe("orders-db.sigma.internal");
  });

  // The demo used to print 5432/3306/27017/6379 — the engines' CONTAINER ports,
  // which nothing outside the container ever dials. The product publishes a
  // managed engine on a port its server allocated from MESH_PORT_BASE, so an
  // operator who wrote the old number down had to learn it twice.
  it("publishes an ALLOCATED mesh port, never the engine's container port", () => {
    for (const [kind, containerPort] of [
      ["postgres", 5432],
      ["mysql", 3306],
      ["mongodb", 27017],
      ["redis", 6379],
    ] as const) {
      const info = demoDatabaseInfo({ ...RES, kind })!;
      expect(info.port).not.toBe(containerPort);
      expect(info.port).toBeGreaterThanOrEqual(MESH_PORT_BASE);
    }
  });

  // The port belongs to the RESOURCE, not to the engine: the allocator hands
  // out one number per resource whatever is running behind it.
  it("gives one resource one port, whichever engine it turns out to be", () => {
    const ports = DB_ENGINE_KINDS.map((kind) => demoDatabaseInfo({ ...RES, kind })!.port);
    expect(new Set(ports).size).toBe(1);
  });

  it("gives two resources on the same host different ports", () => {
    const a = demoDatabaseInfo({ ...RES, kind: "postgres" })!;
    const b = demoDatabaseInfo({ ...RES, resourceId: "res_99887766", kind: "postgres" })!;
    expect(a.port).not.toBe(b.port);
  });

  // The image is the one fact on this panel a reader plans around — it is
  // rendered under a label reading "Engine" — and the demo used to state a
  // different major version of Postgres than the control plane pins.
  it("names the image the control plane actually pins, for every engine", () => {
    for (const kind of DB_ENGINE_KINDS) {
      expect(demoDatabaseInfo({ ...RES, kind })!.image).toBe(DB_ENGINE_CATALOG[kind].image);
    }
  });

  // The wizard shows these once and the resource page shows them for as long as
  // the resource lives. Two different answers would make one of the two wrong.
  it("answers the same for the same resource, every time", () => {
    const a = demoDatabaseConnection({ ...RES, kind: "postgres" })!;
    const b = demoDatabaseConnection({ ...RES, kind: "postgres" })!;
    expect(a).toEqual(b);
  });

  it("answers differently for different resources", () => {
    const a = demoDatabaseConnection({ ...RES, kind: "postgres" })!;
    const b = demoDatabaseConnection({
      ...RES,
      resourceId: "res_99887766",
      kind: "postgres",
    })!;
    expect(a.password).not.toBe(b.password);
    expect(a.username).not.toBe(b.username);
  });

  // The prefix travels with the value into whatever it is pasted into, which a
  // label beside the field would not.
  it("marks every secret as a demo value in the value itself", () => {
    const conn = demoDatabaseConnection({ ...RES, kind: "postgres" })!;
    expect(conn.password.startsWith("demo_")).toBe(true);
    expect(conn.url).toContain("demo_");
  });

  // Each URL is spelled out rather than pattern-matched, because the shapes are
  // exactly what drifted: the demo appended ?sslmode=disable to the Postgres
  // URL and put the database in the MongoDB path, and the control plane renders
  // neither. Both sides fill one template now (store.DBEngineDef.URLTemplate),
  // so a shape edit that reaches only one of them cannot survive.
  it("builds the URL the control plane would have handed out", () => {
    const pg = demoDatabaseConnection({ ...RES, kind: "postgres" })!;
    expect(pg.url).toBe(
      `postgresql://${pg.username}:${pg.password}@10.8.0.21:${pg.port}/orders_db`
    );

    const my = demoDatabaseConnection({ ...RES, kind: "mysql" })!;
    expect(my.url).toBe(`mysql://${my.username}:${my.password}@10.8.0.21:${my.port}/orders_db`);

    // Credentials live on the admin database, so authSource is what makes this
    // URL authenticate; the database name is not in the path at all.
    const mongo = demoDatabaseConnection({ ...RES, kind: "mongodb" })!;
    expect(mongo.url).toBe(
      `mongodb://${mongo.username}:${mongo.password}@10.8.0.21:${mongo.port}/?authSource=admin`
    );
    expect(mongo.url).not.toContain("orders_db");
  });

  // Redis has no database in the sense the others do, and inventing one would
  // produce a URL that does not connect.
  it("gives redis a password-only URL, with no invented database name", () => {
    const conn = demoDatabaseConnection({ ...RES, kind: "redis" })!;
    expect(conn.url).toBe(`redis://:${conn.password}@10.8.0.21:${conn.port}/0`);
    expect(conn.url).not.toContain(conn.username);
    expect(conn.url).not.toContain("orders_db");
  });

  // Container and DNS names cannot carry a dash where a database name goes.
  it("turns a dashed resource name into a usable database name", () => {
    expect(demoDatabaseInfo({ ...RES, kind: "postgres" })!.database).toBe("orders_db");
  });
});

describe("a demo object store's connection details", () => {
  it("publishes an endpoint on the mesh address and the allocated port", () => {
    const info = demoS3Info({ resourceId: "res_s3", resourceName: "assets", meshIp: "10.8.0.9" });
    expect(info.endpoint).toBe(`http://10.8.0.9:${info.port}`);
    expect(info.meshOnly).toBe(true);
  });

  // 9000 is MinIO's port INSIDE the container and not SeaweedFS's at all; the
  // number an S3 client dials is the one the server's allocator handed out,
  // which is the same space the databases draw from.
  it("dials an allocated mesh port, not the engine's API port", () => {
    for (const engine of ["minio", "seaweedfs"]) {
      const info = demoS3Info({ resourceId: "res_s3", resourceName: "assets", engine });
      expect(info.port).toBeGreaterThanOrEqual(MESH_PORT_BASE);
      expect(info.port).not.toBe(9000);
      expect(info.port).not.toBe(8333);
    }
  });

  it("names each engine's pinned image", () => {
    for (const engine of ["minio", "seaweedfs"] as const) {
      const info = demoS3Info({ resourceId: "res_s3", resourceName: "assets", engine });
      expect(info.image).toBe(S3_ENGINE_CATALOG[engine].image);
    }
  });

  it("marks the secret key as a demo value and leaves the access key readable", () => {
    const conn = demoS3Connection({ resourceId: "res_s3", resourceName: "assets" });
    expect(conn.secretKey.startsWith("demo_")).toBe(true);
    expect(conn.accessKey).toBe(conn.accessKey.toUpperCase());
  });

  it("answers the same for the same resource, every time", () => {
    const a = demoS3Connection({ resourceId: "res_s3", resourceName: "assets" });
    const b = demoS3Connection({ resourceId: "res_s3", resourceName: "assets" });
    expect(a).toEqual(b);
  });

  // An engine the catalog does not know still gets an endpoint: the resource
  // exists either way, and refusing to describe it would be a blank panel.
  it("falls back to the default engine's image for an unknown engine", () => {
    const info = demoS3Info({ resourceId: "res_s3", resourceName: "assets", engine: "ceph" });
    expect(info.engine).toBe("ceph");
    expect(info.image).toBe(S3_ENGINE_CATALOG.minio.image);
    expect(info.endpoint).toBe(`http://assets.sigma.internal:${info.port}`);
  });
});
