import { describe, expect, it } from "vitest";
import {
  blankPortMapping,
  defaultHealthPath,
  defaultHealthPort,
  defaultPortMappings,
  domainError,
  healthCheckSpec,
  portMappingsError,
  reachability,
  specPorts,
} from "./networking";

describe("detected ports become editable mappings", () => {
  // Host 0 is the safe default twice over: a published port collides the moment
  // two apps want 3000, and it bypasses the proxy that terminates TLS.
  it("defaults every detected port to internal only", () => {
    const mappings = defaultPortMappings([3000, 8080]);
    expect(mappings.map((m) => [m.container, m.host])).toEqual([
      [3000, 0],
      [8080, 0],
    ]);
  });

  it("drops nonsense and duplicates rather than carrying them to the spec", () => {
    expect(defaultPortMappings([0, 70000, 3000, 3000, -1]).map((m) => m.container)).toEqual([3000]);
  });

  it("handles a repository that declares none", () => {
    expect(defaultPortMappings(undefined)).toEqual([]);
  });

  it("gives every mapping a distinct id, so a row can be removed", () => {
    const a = blankPortMapping();
    const b = blankPortMapping();
    expect(a.id).not.toBe(b.id);
  });
});

describe("port validation", () => {
  it("accepts internal-only and a published port", () => {
    expect(portMappingsError([{ id: "1", container: 3000, host: 0 }])).toBeNull();
    expect(portMappingsError([{ id: "1", container: 3000, host: 8080 }])).toBeNull();
  });

  it("refuses an out-of-range port", () => {
    expect(portMappingsError([{ id: "1", container: 0, host: 0 }])).toContain("Container port");
    expect(portMappingsError([{ id: "1", container: 3000, host: 99999 }])).toContain("Host port");
  });

  it("refuses two mappings fighting over one host port", () => {
    expect(
      portMappingsError([
        { id: "1", container: 3000, host: 8080 },
        { id: "2", container: 4000, host: 8080 },
      ])
    ).toContain("same host port");
  });

  it("does not mistake two internal-only ports for a collision", () => {
    expect(
      portMappingsError([
        { id: "1", container: 3000, host: 0 },
        { id: "2", container: 4000, host: 0 },
      ])
    ).toBeNull();
  });

  it("refuses the same container port twice", () => {
    expect(
      portMappingsError([
        { id: "1", container: 3000, host: 0 },
        { id: "2", container: 3000, host: 8080 },
      ])
    ).toContain("mapped twice");
  });
});

describe("what reaches the resource spec", () => {
  // Shape matches the reconciler's appResourceSpec.Ports exactly; a renamed
  // field here silently unpublishes an app rather than failing.
  it("emits container/host/protocol", () => {
    expect(specPorts([{ id: "1", container: 3000, host: 8080 }])).toEqual([
      { container: 3000, host: 8080, protocol: "tcp" },
    ]);
  });

  it("skips a half-typed row instead of sending port 0", () => {
    expect(specPorts([{ id: "1", container: 0, host: 0 }])).toEqual([]);
  });
});

describe("the health check is pre-filled, not guessed at deploy time", () => {
  it("uses a detected HTTP path", () => {
    expect(defaultHealthPath({ type: "http", path: "/healthz" })).toBe("/healthz");
  });

  it("offers a correctable default when nothing was declared", () => {
    expect(defaultHealthPath({ type: "tcp", port: 3000 })).toBe("/");
    expect(defaultHealthPath(undefined)).toBe("/");
  });

  it("targets the detected port, or the first mapping", () => {
    expect(defaultHealthPort({ type: "http", port: 8080 }, [])).toBe(8080);
    expect(defaultHealthPort(undefined, [{ id: "1", container: 3000, host: 0 }])).toBe(3000);
  });

  it("builds an http probe and keeps the repository's own interval", () => {
    expect(
      healthCheckSpec({ path: "/healthz", port: 3000, detected: { intervalSec: 30 } })
    ).toEqual({ type: "http", path: "/healthz", port: 3000, intervalSec: 30 });
  });

  it("normalizes a path typed without its slash", () => {
    expect(healthCheckSpec({ path: "healthz", port: 3000 }).path).toBe("/healthz");
  });

  // Clearing the field is a request for a TCP probe, not for an HTTP probe
  // against "".
  it("falls back to TCP when the path is cleared", () => {
    expect(healthCheckSpec({ path: "  ", port: 3000 })).toEqual({ type: "tcp", port: 3000 });
  });
});

describe("domains", () => {
  it("accepts a hostname", () => {
    expect(domainError("app.example.com")).toBeNull();
    expect(domainError("")).toBeNull();
  });

  it("catches the paste people actually make", () => {
    expect(domainError("https://app.example.com")).toContain("without http");
    expect(domainError("app.example.com/path")).toContain("without a path");
    expect(domainError("not a host")).toContain("hostname");
  });
});

describe("reachability is stated out loud", () => {
  // A user who published nothing and attached no domain has deployed something
  // they cannot reach; they should find that out here rather than by curling it.
  it("says internal-only when nothing is exposed", () => {
    const r = reachability([{ id: "1", container: 3000, host: 0 }], "");
    expect(r.reachable).toBe(false);
    expect(r.summary).toContain("Internal only");
  });

  it("names the domain when one is attached", () => {
    expect(reachability([{ id: "1", container: 3000, host: 0 }], "app.example.com")).toEqual({
      reachable: true,
      summary: "Reachable at https://app.example.com",
    });
  });

  it("names published host ports", () => {
    expect(reachability([{ id: "1", container: 3000, host: 8080 }], "").summary).toContain("8080");
  });
});
