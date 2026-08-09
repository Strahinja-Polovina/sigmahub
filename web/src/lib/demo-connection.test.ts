import { describe, expect, it } from "vitest";

import {
  demoDatabaseConnection,
  demoDatabaseInfo,
  demoS3Connection,
  demoS3Info,
  isDemoDatabaseKind,
} from "./demo-connection";
import { RESOURCE_KINDS } from "./server-catalog.generated";

const RES = { resourceId: "res_a1b2c3d4", resourceName: "orders-db", meshIp: "10.8.0.21" };

describe("which kinds are managed database engines", () => {
  it("answers for every kind in the catalog, with no fall-through", () => {
    const managed = RESOURCE_KINDS.filter((k) => isDemoDatabaseKind(k));
    expect(managed.sort()).toEqual(["mongodb", "mysql", "postgres", "redis"]);
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
    expect(info.port).toBe(5432);
  });

  // Not a placeholder: it is the name the container answers to on the mesh
  // before the tunnel is up, which is what an operator would actually type.
  it("falls back to the resource's mesh name when the host has no address yet", () => {
    const info = demoDatabaseInfo({ ...RES, meshIp: "", kind: "redis" })!;
    expect(info.host).toBe("orders-db.sigma.internal");
  });

  it("gives each engine its own port and image", () => {
    const ports = ["postgres", "mysql", "mongodb", "redis"].map(
      (kind) => demoDatabaseInfo({ ...RES, kind })!.port
    );
    expect(new Set(ports).size).toBe(4);
    const images = ["postgres", "mysql", "mongodb", "redis"].map(
      (kind) => demoDatabaseInfo({ ...RES, kind })!.image
    );
    expect(new Set(images).size).toBe(4);
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

  it("builds a URL each engine's own client would accept", () => {
    expect(demoDatabaseConnection({ ...RES, kind: "postgres" })!.url).toMatch(
      /^postgresql:\/\/[^@]+@10\.8\.0\.21:5432\/orders_db\?/
    );
    expect(demoDatabaseConnection({ ...RES, kind: "mysql" })!.url).toMatch(
      /^mysql:\/\/[^@]+@10\.8\.0\.21:3306\/orders_db$/
    );
    expect(demoDatabaseConnection({ ...RES, kind: "mongodb" })!.url).toContain("authSource=admin");
  });

  // Redis has no database in the sense the others do, and inventing one would
  // produce a URL that does not connect.
  it("gives redis a password-only URL, with no invented database name", () => {
    const url = demoDatabaseConnection({ ...RES, kind: "redis" })!.url;
    expect(url.startsWith("redis://:")).toBe(true);
    expect(url).not.toContain("orders_db");
  });

  // Container and DNS names cannot carry a dash where a database name goes.
  it("turns a dashed resource name into a usable database name", () => {
    expect(demoDatabaseInfo({ ...RES, kind: "postgres" })!.database).toBe("orders_db");
  });
});

describe("a demo object store's connection details", () => {
  it("publishes an endpoint on the mesh address", () => {
    const info = demoS3Info({ resourceId: "res_s3", resourceName: "assets", meshIp: "10.8.0.9" });
    expect(info.endpoint).toBe("http://10.8.0.9:9000");
    expect(info.meshOnly).toBe(true);
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
    expect(info.image).toContain("minio");
  });
});
