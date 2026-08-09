import { describe, expect, it } from "vitest";
import { blockingGaps, reviewSummary, type ReviewInput } from "./review";

const app: ReviewInput = {
  kind: "app",
  name: "storefront",
  repo: "acme/storefront",
  branch: "main",
  buildMethod: "dockerfile",
  dockerfile: "Dockerfile",
  contextSubdir: "apps/api",
  ports: [{ id: "1", container: 3000, host: 0 }],
  domain: "shop.acme.com",
  healthPath: "/healthz",
  envVarCount: 4,
  projectName: "Webshop",
  environmentName: "production",
  serverName: "hel-general-01",
};

function rowFor(input: ReviewInput, label: string) {
  return reviewSummary(input).find((r) => r.label === label);
}

describe("the review restates every decision", () => {
  it("shows the source at its branch", () => {
    expect(rowFor(app, "Source")?.value).toBe("acme/storefront @ main");
  });

  it("shows the build method with its paths", () => {
    const row = rowFor(app, "Build");
    expect(row?.value).toBe("Dockerfile");
    expect(row?.hint).toContain("apps/api");
  });

  it("counts compose services rather than naming a Dockerfile", () => {
    const row = rowFor(
      { ...app, buildMethod: "compose", dockerfile: "", composeServiceCount: 4 },
      "Build"
    );
    expect(row?.value).toBe("Docker Compose");
    expect(row?.hint).toContain("4 services");
  });

  it("shows ports with their reachability", () => {
    const row = rowFor(app, "Ports");
    expect(row?.value).toBe("3000");
    expect(row?.hint).toContain("shop.acme.com");
  });

  it("shows a published mapping as host→container", () => {
    const row = rowFor({ ...app, ports: [{ id: "1", container: 3000, host: 8080 }] }, "Ports");
    expect(row?.value).toBe("8080→3000");
  });

  it("shows the target, server or cluster", () => {
    expect(rowFor(app, "Target")?.hint).toBe("hel-general-01");
    expect(rowFor({ ...app, serverName: undefined, clusterName: "prod" }, "Target")?.hint).toBe(
      "cluster prod"
    );
  });

  it("counts the variables", () => {
    expect(rowFor(app, "Variables")?.value).toBe("4 variables");
    expect(rowFor({ ...app, envVarCount: 0 }, "Variables")?.value).toBe("none");
  });

  // A review screen listing "Domain: —" invites the reading that a domain was
  // configured and is empty, when the truth is that the flow never asked.
  it("omits what the flow never asked about", () => {
    const redis: ReviewInput = {
      kind: "redis",
      name: "cache-prod",
      projectName: "Webshop",
      environmentName: "production",
      serverName: "hel-db-01",
    };
    const labels = reviewSummary(redis).map((r) => r.label);
    expect(labels).not.toContain("Source");
    expect(labels).not.toContain("Build");
    expect(labels).not.toContain("Ports");
    expect(labels).not.toContain("Variables");
    expect(labels).toEqual(["Type", "Name", "Target"]);
  });

  it("gives every row the step that set it, so it can be corrected", () => {
    for (const row of reviewSummary(app)) {
      expect(row.step, row.label).toBeTruthy();
    }
  });
});

describe("what still blocks the create call", () => {
  it("passes a complete application", () => {
    expect(blockingGaps(app)).toEqual([]);
  });

  it("names each missing piece rather than greying out a button", () => {
    const gaps = blockingGaps({ kind: "app", name: "" });
    expect(gaps).toContain("The resource needs a name.");
    expect(gaps).toContain("Pick a project.");
    expect(gaps).toContain("Pick a server or a cluster.");
    expect(gaps).toContain("Pick a repository.");
  });

  it("does not ask a database for a repository", () => {
    const gaps = blockingGaps({
      kind: "postgres",
      name: "db",
      projectName: "p",
      environmentName: "e",
      serverName: "s",
    });
    expect(gaps).toEqual([]);
  });

  // An endpoint with no model has nothing to serve, and the control plane
  // refuses the create — better to say so here than after the wizard closed.
  it("requires a model for an inference endpoint", () => {
    expect(
      blockingGaps({
        kind: "llm",
        name: "llama",
        projectName: "p",
        environmentName: "e",
        serverName: "s",
      })
    ).toContain("Name the model this endpoint serves.");
  });

  it("accepts a cluster as the target", () => {
    expect(
      blockingGaps({
        kind: "app",
        name: "api",
        repo: "acme/api",
        buildMethod: "compose",
        projectName: "p",
        environmentName: "e",
        clusterName: "prod",
      })
    ).toEqual([]);
  });
});
