import { describe, expect, it } from "vitest";
import {
  buildInventory,
  clusterOptions,
  kindAvailability,
  serverIsDeployable,
  serverOptions,
  type WizardProject,
} from "./availability";
import type { ModelCard } from "./llm";

function projectWith(types: string[], status?: string): WizardProject[] {
  return [
    {
      id: "prj",
      name: "Project",
      environments: [
        {
          id: "env",
          name: "production",
          servers: types.map((type, i) => ({
            id: `srv_${i}`,
            name: `host-${i}`,
            type,
            status,
          })),
        },
      ],
    },
  ];
}

describe("a kind with zero compatible targets says so on step one", () => {
  it("offers object storage only when a Storage server exists", () => {
    const none = buildInventory(projectWith(["general", "database"]));
    const with3 = buildInventory(projectWith(["general", "storage"]));
    expect(kindAvailability("s3", none).available).toBe(false);
    expect(kindAvailability("s3", with3).available).toBe(true);
  });

  it("names what to connect and links to where", () => {
    const inv = buildInventory(projectWith(["general"]));
    const verdict = kindAvailability("s3", inv);
    expect(verdict.reason).toContain("Storage");
    expect(verdict.action?.href).toBe("/dashboard/servers");
  });

  // The LLM path was the worst of these: it asked for a runtime and a model
  // reference before mentioning that nothing here has a GPU.
  it("explains the GPU requirement in hardware terms, not matrix terms", () => {
    const inv = buildInventory(projectWith(["general", "database", "storage"]));
    const verdict = kindAvailability("llm", inv);
    expect(verdict.available).toBe(false);
    expect(verdict.reason).toContain("GPU");
    expect(verdict.reason).not.toContain("General");
    expect(verdict.action?.href).toBe("/dashboard/servers");
  });

  it("counts a cluster as a target for an app, and never for a database", () => {
    const inv = buildInventory(
      projectWith(["build"]),
      [{ id: "cl", name: "prod", environmentId: "env" }],
      ["postgres", "mysql", "redis", "mongodb", "s3"]
    );
    expect(kindAvailability("app", inv).available).toBe(true);
    expect(kindAvailability("postgres", inv).available).toBe(false);
    expect(kindAvailability("postgres", inv).reason).toContain("cluster");
  });

  // A host the enrollment gate refused matches the matrix on paper and not in
  // fact, and the control plane refuses to schedule onto it — counting it here
  // would put the operator back in the dead end one screen later (SIGMA-203).
  it("does not count a server the enrollment gate refused", () => {
    const inv = buildInventory(projectWith(["gpu"], "incompatible"));
    expect(kindAvailability("llm", inv).available).toBe(false);
  });

  it("does not count a server on its way out", () => {
    expect(serverIsDeployable({ id: "s", name: "s", type: "gpu", status: "decommissioning" })).toBe(
      false
    );
    expect(serverIsDeployable({ id: "s", name: "s", type: "gpu", status: "running" })).toBe(true);
  });
});

describe("per-server eligibility carries its reason", () => {
  const env = {
    id: "env",
    name: "production",
    servers: [
      { id: "a", name: "general-1", type: "general", status: "running" },
      { id: "b", name: "build-1", type: "build", status: "running" },
      { id: "c", name: "gpu-1", type: "gpu", status: "incompatible" },
    ],
  };

  it("allows a compatible server", () => {
    const opts = serverOptions(env, "app");
    expect(opts.find((o) => o.server.id === "a")?.eligible).toBe(true);
  });

  // A greyed-out row whose cause is a matrix the operator has never seen is
  // the pattern the rebuild removes.
  it("says why a build server cannot host an app", () => {
    const opts = serverOptions(env, "app");
    const build = opts.find((o) => o.server.id === "b");
    expect(build?.eligible).toBe(false);
    expect(build?.reason).toContain("Build server cannot host");
  });

  it("distinguishes a refused HOST from an incompatible TYPE", () => {
    const opts = serverOptions(env, "app");
    const gpu = opts.find((o) => o.server.id === "c");
    expect(gpu?.eligible).toBe(false);
    // Its type CAN host an app; the machine is what was refused.
    expect(gpu?.reason).toContain("refused as a GPU server");
  });
});

describe("a GPU server too small for the chosen model is refused with both sizes", () => {
  const GB = 1_000_000_000;

  function modelCard(over: Partial<ModelCard> = {}): ModelCard {
    return {
      id: "meta-llama/Llama-3.1-70B-Instruct",
      name: "Llama 3.1 70B Instruct",
      gated: false,
      downloads: 421_760,
      likes: 1_205,
      pipelineTag: "text-generation",
      library: "transformers",
      engine: "vllm",
      parameters: 70_553_706_496,
      parametersKnown: true,
      quantization: "none",
      bytesPerParam: 2,
      vramBytesRequired: 188_143_217_323,
      vramText: "~188 GB",
      sizingBasis: "safetensors",
      ...over,
    };
  }

  const envWithCards = {
    id: "env",
    name: "production",
    servers: [
      {
        id: "big",
        name: "gpu-a100-01",
        type: "gpu",
        status: "running",
        gpu: { vendor: "nvidia", count: 4, vramBytesPerGpu: 80 * GB },
      },
      {
        id: "small",
        name: "gpu-l4-01",
        type: "gpu",
        status: "running",
        gpu: { vendor: "nvidia", count: 1, vramBytesPerGpu: 24 * GB },
      },
      // The fleet's older agent: it heartbeats, it hosts, it has simply never
      // reported an inventory.
      { id: "silent", name: "gpu-old-01", type: "gpu", status: "running" },
    ],
  };

  it("refuses the card that cannot hold the model, and says by how much", () => {
    const opts = serverOptions(envWithCards, "llm", modelCard());
    const small = opts.find((o) => o.server.id === "small");
    expect(small?.eligible).toBe(false);
    expect(small?.reason).toBe(
      "This model needs about 188 GB of VRAM; this server's GPU has 24 GB."
    );
  });

  // Four 80 GB cards is 320 GB of VRAM and still cannot run a 188 GB model:
  // the runtime loads it into one card. A filter that added them up would
  // promise a deploy that OOMs.
  it("compares against one card, not the host's total", () => {
    const opts = serverOptions(envWithCards, "llm", modelCard());
    expect(opts.find((o) => o.server.id === "big")?.eligible).toBe(false);
  });

  it("keeps every server eligible before a model is chosen", () => {
    const opts = serverOptions(envWithCards, "llm");
    expect(opts.every((o) => o.eligible)).toBe(true);
  });

  it("keeps a server whose agent never reported a GPU — absent is unknown", () => {
    const opts = serverOptions(envWithCards, "llm", modelCard());
    expect(opts.find((o) => o.server.id === "silent")?.eligible).toBe(true);
  });

  it("keeps every server when the model's size is unknown", () => {
    const unsized = modelCard({
      parametersKnown: false,
      parameters: 0,
      vramBytesRequired: 0,
      vramText: "",
      sizingBasis: "unknown",
    });
    expect(serverOptions(envWithCards, "llm", unsized).every((o) => o.eligible)).toBe(true);
  });

  it("offers the servers that can hold a smaller model", () => {
    const small = modelCard({
      id: "meta-llama/Llama-3.1-8B-Instruct",
      parameters: 8_030_261_248,
      vramBytesRequired: 21_414_029_995,
      vramText: "~21 GB",
    });
    expect(serverOptions(envWithCards, "llm", small).every((o) => o.eligible)).toBe(true);
  });

  // The host is the wrong shape for the job regardless of which model was
  // picked, and "a General server cannot host a Model endpoint" is the fact
  // that gets them somewhere.
  it("still leads with the type reason when the type is wrong", () => {
    const env = {
      id: "env",
      name: "production",
      servers: [{ id: "gen", name: "general-1", type: "general", status: "running" }],
    };
    expect(serverOptions(env, "llm", modelCard())[0].reason).toContain("cannot host");
  });
});

describe("cluster options are scoped to the environment", () => {
  const clusters = [
    { id: "cl_a", name: "prod", environmentId: "env_a" },
    { id: "cl_b", name: "staging", environmentId: "env_b" },
  ];
  const inv = buildInventory([], clusters, ["postgres"]);

  // A cluster belongs to exactly one environment and the control plane says so;
  // offering the others would offer a target it refuses.
  it("offers only the chosen environment's cluster", () => {
    const opts = clusterOptions(clusters, "env_a", "app", inv);
    expect(opts.map((o) => o.cluster.id)).toEqual(["cl_a"]);
  });

  it("refuses an excluded kind with the reason", () => {
    const opts = clusterOptions(clusters, "env_a", "postgres", inv);
    expect(opts[0].eligible).toBe(false);
    expect(opts[0].reason).toContain("one host");
  });

  it("refuses a cluster that is still coming up", () => {
    const opts = clusterOptions(
      [{ id: "cl_a", name: "prod", environmentId: "env_a", status: "provisioning" }],
      "env_a",
      "app",
      inv
    );
    expect(opts[0].eligible).toBe(false);
  });
});
