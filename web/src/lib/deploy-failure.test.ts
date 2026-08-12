import { describe, expect, it } from "vitest";

import { classifyDeployFailure } from "./deploy-failure";

// SIGMA-353: the agent already reports the real cause; this turns it into a
// named category and a specific next action, so a health-check timeout and a
// missing registry credential stop reading as the same generic "failed".

describe("classifyDeployFailure", () => {
  const cases: { error: string; category: string; remediationMatch: RegExp }[] = [
    {
      error: "pull from ghcr.io/acme/api:latest needs a registry credential and this agent has no way to fetch one",
      category: "registry",
      remediationMatch: /connect the registry/i,
    },
    {
      error: "image.build: build context missing Dockerfile (clone did not run?)",
      category: "build",
      remediationMatch: /build output/i,
    },
    {
      error: "health gate timed out after 2m0s: health check /healthz returned 502",
      category: "health",
      remediationMatch: /listens on the declared port/i,
    },
    {
      error: "start container: driver failed programming external connectivity: address already in use",
      category: "port",
      remediationMatch: /change the published port/i,
    },
    {
      error: "volume.ensure: no space left on device",
      category: "volume",
      remediationMatch: /free disk/i,
    },
    {
      error: "network.ensure: failed to create network sigmahub_proj",
      category: "network",
      remediationMatch: /agent is healthy/i,
    },
  ];

  for (const c of cases) {
    it(`classifies ${c.category}`, () => {
      const r = classifyDeployFailure(c.error);
      expect(r.category).toBe(c.category);
      expect(r.title).toBeTruthy();
      expect(r.remediation).toMatch(c.remediationMatch);
    });
  }

  it("registry beats the generic image case when both phrases are present", () => {
    // "needs a registry credential" also contains "pull" — the specific one wins.
    expect(classifyDeployFailure("pull image failed: needs a registry credential").category).toBe(
      "registry"
    );
  });

  it("falls back to a generic-but-actionable message for an unrecognized error", () => {
    const r = classifyDeployFailure("something nobody has a matcher for yet");
    expect(r.category).toBe("unknown");
    expect(r.remediation).toMatch(/Deployments tab/i);
  });

  it("empty error is the generic case, never a crash", () => {
    expect(classifyDeployFailure("").category).toBe("unknown");
    expect(classifyDeployFailure(null).category).toBe("unknown");
    expect(classifyDeployFailure(undefined).category).toBe("unknown");
  });
});
