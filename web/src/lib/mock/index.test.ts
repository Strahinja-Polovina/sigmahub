import { describe, it, expect } from "vitest";
import {
  getOrgs,
  getOrg,
  getProjects,
  getProject,
  getEnvironments,
  getEnvironment,
  getServers,
  getServer,
  getResources,
  getResourcesByProject,
  getResource,
  getMembers,
  canHost,
  availabilityMatrix,
  getMetrics,
  getLogs,
  getDeployments,
  getBillingSummary,
  UNIT_PRICE,
  FREE_TIER_SERVERS,
  CURRENCY,
} from "./index";

describe("constants", () => {
  it("UNIT_PRICE is 5", () => {
    expect(UNIT_PRICE).toBe(5);
  });

  it("FREE_TIER_SERVERS is 3", () => {
    expect(FREE_TIER_SERVERS).toBe(3);
  });

  it("CURRENCY is EUR", () => {
    expect(CURRENCY).toBe("EUR");
  });
});

describe("getOrgs", () => {
  it("returns all orgs", () => {
    const result = getOrgs();
    expect(result.length).toBeGreaterThanOrEqual(2);
    expect(result.map((o) => o.id)).toContain("org_acme");
    expect(result.map((o) => o.id)).toContain("org_northwind");
  });
});

describe("getOrg", () => {
  it("returns the org by id", () => {
    const org = getOrg("org_acme");
    expect(org.name).toBe("Acme Cloud");
  });

  it("falls back to the first org for unknown id", () => {
    const org = getOrg("org_unknown");
    expect(org).toBeDefined();
    expect(org.id).toBe("org_acme");
  });
});

describe("getProjects", () => {
  it("returns projects for acme org", () => {
    const result = getProjects("org_acme");
    expect(result.length).toBe(3);
    result.forEach((p) => expect(p.orgId).toBe("org_acme"));
  });

  it("returns projects for northwind org", () => {
    const result = getProjects("org_northwind");
    expect(result.length).toBe(1);
    expect(result[0].name).toBe("Marketing Site");
  });

  it("returns empty for unknown org", () => {
    expect(getProjects("org_nope")).toEqual([]);
  });
});

describe("getProject", () => {
  it("returns a project by id", () => {
    const p = getProject("proj_webshop");
    expect(p).toBeDefined();
    expect(p!.name).toBe("Webshop");
  });

  it("returns undefined for unknown id", () => {
    expect(getProject("proj_nope")).toBeUndefined();
  });
});

describe("getEnvironments", () => {
  it("returns environments for a project", () => {
    const envs = getEnvironments("proj_webshop");
    expect(envs.length).toBe(2);
    envs.forEach((e) => expect(e.projectId).toBe("proj_webshop"));
  });

  it("returns empty for unknown project", () => {
    expect(getEnvironments("proj_nope")).toEqual([]);
  });
});

describe("getEnvironment", () => {
  it("returns an environment by id", () => {
    const e = getEnvironment("env_webshop_prod");
    expect(e).toBeDefined();
    expect(e!.name).toBe("production");
  });

  it("returns undefined for unknown id", () => {
    expect(getEnvironment("env_nope")).toBeUndefined();
  });
});

describe("getServers", () => {
  it("returns servers for acme org", () => {
    const srvs = getServers("org_acme");
    expect(srvs.length).toBeGreaterThanOrEqual(5);
    srvs.forEach((s) => expect(s.orgId).toBe("org_acme"));
  });

  it("returns servers for northwind org", () => {
    const srvs = getServers("org_northwind");
    expect(srvs.length).toBe(1);
  });

  it("returns empty for unknown org", () => {
    expect(getServers("org_nope")).toEqual([]);
  });
});

describe("getServer", () => {
  it("returns a server by id", () => {
    const s = getServer("srv_gen_1");
    expect(s).toBeDefined();
    expect(s!.name).toBe("hel-general-01");
  });

  it("returns undefined for unknown id", () => {
    expect(getServer("srv_nope")).toBeUndefined();
  });
});

describe("getResources", () => {
  it("returns resources for an environment", () => {
    const res = getResources("env_webshop_prod");
    expect(res.length).toBe(3);
    res.forEach((r) => expect(r.environmentId).toBe("env_webshop_prod"));
  });

  it("returns empty for unknown environment", () => {
    expect(getResources("env_nope")).toEqual([]);
  });
});

describe("getResourcesByProject", () => {
  it("returns resources for a project", () => {
    const res = getResourcesByProject("proj_webshop");
    expect(res.length).toBeGreaterThanOrEqual(3);
    res.forEach((r) => expect(r.projectId).toBe("proj_webshop"));
  });

  it("returns empty for unknown project", () => {
    expect(getResourcesByProject("proj_nope")).toEqual([]);
  });
});

describe("getResource", () => {
  it("returns a resource by id", () => {
    const r = getResource("res_web");
    expect(r).toBeDefined();
    expect(r!.name).toBe("storefront");
  });

  it("returns undefined for unknown id", () => {
    expect(getResource("res_nope")).toBeUndefined();
  });
});

describe("getMembers", () => {
  it("returns the members list", () => {
    const m = getMembers("org_acme");
    expect(m.length).toBe(4);
    expect(m[0].name).toBe("Strahinja Polovina");
  });
});

describe("availabilityMatrix / canHost", () => {
  it("general servers can host apps", () => {
    expect(canHost("general", "app")).toBe(true);
  });

  it("general servers can host postgres", () => {
    expect(canHost("general", "postgres")).toBe(true);
  });

  it("general servers cannot host s3", () => {
    expect(canHost("general", "s3")).toBe(false);
  });

  it("storage servers can host s3", () => {
    expect(canHost("storage", "s3")).toBe(true);
  });

  it("storage servers cannot host apps", () => {
    expect(canHost("storage", "app")).toBe(false);
  });

  it("database servers can host postgres", () => {
    expect(canHost("database", "postgres")).toBe(true);
  });

  it("database servers cannot host apps", () => {
    expect(canHost("database", "app")).toBe(false);
  });

  it("gpu servers can host llm", () => {
    expect(canHost("gpu", "llm")).toBe(true);
  });

  it("gpu servers can host apps", () => {
    expect(canHost("gpu", "app")).toBe(true);
  });

  it("gpu servers cannot host postgres", () => {
    expect(canHost("gpu", "postgres")).toBe(false);
  });

  it("covers every server type", () => {
    const types = Object.keys(availabilityMatrix);
    expect(types).toEqual(
      expect.arrayContaining(["general", "database", "storage", "gpu"]),
    );
  });
});

describe("getMetrics", () => {
  it("returns 24 data points by default", () => {
    const m = getMetrics("test-key");
    expect(m).toHaveLength(24);
  });

  it("returns the requested number of points", () => {
    const m = getMetrics("key", 10);
    expect(m).toHaveLength(10);
  });

  it("each point has t, cpu, mem, net", () => {
    const m = getMetrics("key");
    for (const p of m) {
      expect(p).toHaveProperty("t");
      expect(p).toHaveProperty("cpu");
      expect(p).toHaveProperty("mem");
      expect(p).toHaveProperty("net");
      expect(typeof p.cpu).toBe("number");
      expect(typeof p.mem).toBe("number");
      expect(typeof p.net).toBe("number");
    }
  });

  it("is deterministic for the same seed key", () => {
    const a = getMetrics("stable");
    const b = getMetrics("stable");
    expect(a).toEqual(b);
  });

  it("produces different data for different keys", () => {
    const a = getMetrics("alpha");
    const b = getMetrics("beta");
    expect(a).not.toEqual(b);
  });
});

describe("getLogs", () => {
  it("returns 40 lines by default", () => {
    const logs = getLogs("test");
    expect(logs).toHaveLength(40);
  });

  it("returns the requested number of lines", () => {
    const logs = getLogs("test", 5);
    expect(logs).toHaveLength(5);
  });

  it("each line has t, level, msg", () => {
    for (const l of getLogs("test")) {
      expect(l).toHaveProperty("t");
      expect(["info", "warn", "error"]).toContain(l.level);
      expect(typeof l.msg).toBe("string");
    }
  });

  it("is deterministic", () => {
    expect(getLogs("seed")).toEqual(getLogs("seed"));
  });
});

describe("getDeployments", () => {
  it("returns 6 deployments", () => {
    const deps = getDeployments("res_web");
    expect(deps).toHaveLength(6);
  });

  it("first deployment has status running", () => {
    const deps = getDeployments("res_web");
    expect(deps[0].status).toBe("running");
  });

  it("fourth deployment (index 3) has status failed", () => {
    const deps = getDeployments("res_web");
    expect(deps[3].status).toBe("failed");
  });

  it("each deployment has expected fields", () => {
    for (const d of getDeployments("res_api")) {
      expect(d).toHaveProperty("id");
      expect(d).toHaveProperty("resourceId");
      expect(d).toHaveProperty("sha");
      expect(d).toHaveProperty("status");
      expect(d).toHaveProperty("startedAt");
      expect(d).toHaveProperty("durationSec");
      expect(d).toHaveProperty("author");
    }
  });

  it("sha is a 7-char hex string", () => {
    for (const d of getDeployments("res_web")) {
      expect(d.sha).toMatch(/^[0-9a-f]{7}$/);
    }
  });
});

describe("getBillingSummary", () => {
  it("acme org is not on free tier (has > 3 non-provisioning servers)", () => {
    const bill = getBillingSummary("org_acme");
    expect(bill.isFree).toBe(false);
    expect(bill.amount).toBe(bill.connected * UNIT_PRICE);
  });

  it("northwind org is on free tier (1 server)", () => {
    const bill = getBillingSummary("org_northwind");
    expect(bill.connected).toBe(1);
    expect(bill.isFree).toBe(true);
    expect(bill.amount).toBe(0);
  });

  it("returns correct structure", () => {
    const bill = getBillingSummary("org_acme");
    expect(bill).toHaveProperty("connected");
    expect(bill).toHaveProperty("freeTier", FREE_TIER_SERVERS);
    expect(bill).toHaveProperty("unitPrice", UNIT_PRICE);
    expect(bill).toHaveProperty("currency", CURRENCY);
    expect(bill).toHaveProperty("amount");
    expect(bill).toHaveProperty("isFree");
  });

  it("excludes provisioning servers from connected count", () => {
    const bill = getBillingSummary("org_acme");
    const allServers = getServers("org_acme");
    const provisioning = allServers.filter((s) => s.status === "provisioning").length;
    expect(bill.connected).toBe(allServers.length - provisioning);
  });
});
