import { describe, expect, it } from "vitest";
import { RESOURCE_CATEGORIES, RESOURCE_CATEGORY_CATALOG } from "@/lib/server-catalog.generated";
import {
  buildInventory,
  categoryAvailability,
  clusterOptions,
  kindAvailability,
  serverIsDeployable,
  serverOptions,
  targetChoices,
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

const GB = 1_000_000_000;

/** A card as the control plane sends it — a 70B model that fits nothing in
 *  these fixtures unless a test says otherwise. The sizing figures are the CP's
 *  own; nothing here recomputes them. */
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

// Step 1 offers CATEGORIES now, so the contract a kind card has kept since
// SIGMA-207 — an offer that leads nowhere says so where it is offered, with the
// one action that fixes it — has to hold one altitude up. Nothing about it is
// new; what is new is that it can now be true of a card holding four kinds.
describe("a category answers for the kinds inside it", () => {
  it("offers a category as soon as any one of its kinds can be deployed", () => {
    // A general server hosts postgres, mysql, mongodb and redis; a database
    // server hosts the same four. Either way Database opens.
    expect(categoryAvailability("database", buildInventory(projectWith(["general"]))).available).toBe(
      true
    );
    expect(categoryAvailability("application", buildInventory(projectWith(["gpu"]))).available).toBe(
      true
    );
  });

  it("refuses a category whose every kind is refused, and says what to connect", () => {
    const verdict = categoryAvailability("database", buildInventory(projectWith(["storage"])));
    expect(verdict.available).toBe(false);
    // Said once, in the category's own terms, over everything that could have
    // hosted any of its kinds — four per-kind sentences stacked under one card
    // is a wall, not an answer.
    expect(verdict.reason).toContain("General, VPS or Database server");
    expect(verdict.reason).toContain("A Database needs one to run on");
    expect(verdict.reason).not.toContain("PostgreSQL");
    expect(verdict.action?.href).toBe("/dashboard/servers");
  });

  // A category holding one kind IS that kind, and "connect a General server" is
  // not what a missing GPU needs to hear.
  it("keeps a single-kind category's own sentence, verbatim", () => {
    const inv = buildInventory(projectWith(["general", "database"]));
    expect(categoryAvailability("model", inv)).toEqual(kindAvailability("llm", inv));
    expect(categoryAvailability("storage", inv)).toEqual(kindAvailability("s3", inv));
    expect(categoryAvailability("model", inv).reason).toContain("GPU");
  });

  // An available category still holds unavailable kinds, and each keeps its
  // own sentence in the list — the roll-up is about the CARD, not about
  // hiding what is behind it.
  it("leaves the kinds inside an offered category to answer for themselves", () => {
    // A database server hosts every database kind, so Database opens; a fleet
    // with only that has no GPU, so the model endpoint says why not.
    const inv = buildInventory(projectWith(["database"]));
    expect(categoryAvailability("database", inv).available).toBe(true);
    expect(kindAvailability("postgres", inv).available).toBe(true);
    expect(categoryAvailability("model", inv).available).toBe(false);
    expect(kindAvailability("llm", inv).reason).toContain("GPU");
  });

  it("names the cluster only when it refuses the whole category", () => {
    const inv = buildInventory(
      projectWith(["build"]),
      [{ id: "cl", name: "prod", environmentId: "env" }],
      ["postgres", "mysql", "redis", "mongodb", "s3"]
    );
    expect(categoryAvailability("database", inv).reason).toContain("cluster");
    // The cluster hosts an app, so Application is simply available.
    expect(categoryAvailability("application", inv).available).toBe(true);
  });

  // Every card on the screen, empty fleet: none of them may be a dead end with
  // nothing to say, whatever the catalog grows next.
  it("gives every category a reason and an action when nothing is connected", () => {
    const inv = buildInventory([]);
    for (const id of RESOURCE_CATEGORIES) {
      const verdict = categoryAvailability(id, inv);
      expect(verdict.available, id).toBe(false);
      expect(verdict.reason, id).toBeTruthy();
      expect(verdict.action?.href, id).toBe("/dashboard/servers");
      expect(RESOURCE_CATEGORY_CATALOG[id].kinds.length, id).toBeGreaterThan(0);
    }
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

// A cluster was offered for any model at all: it carried no GPU figure, so the
// wizard had nothing to compare and said yes to everything. The operator picked
// a 70B model, targeted the cluster, walked Review and got a 422 from the create
// call — the dead end the whole type-first flow exists to delete, surviving on
// the one target nobody filtered.
describe("a cluster is fit-checked against the model exactly as a server is", () => {
  const inv = buildInventory([], [], []);

  function cluster(maxVramBytesPerGpu?: number) {
    return [{ id: "cl", name: "prod", environmentId: "env", maxVramBytesPerGpu }];
  }

  it("refuses a cluster whose largest node is too small, and states both sizes", () => {
    const [opt] = clusterOptions(cluster(24 * GB), "env", "llm", inv, modelCard());
    expect(opt.eligible).toBe(false);
    expect(opt.reason).toBe(
      "This model needs about 188 GB of VRAM; this cluster's largest GPU node has 24 GB."
    );
  });

  it("offers a cluster whose largest node can hold the model", () => {
    const small = modelCard({
      id: "meta-llama/Llama-3.1-8B-Instruct",
      parameters: 8_030_261_248,
      vramBytesRequired: 21_414_029_995,
      vramText: "~21 GB",
    });
    expect(clusterOptions(cluster(80 * GB), "env", "llm", inv, small).at(0)?.eligible).toBe(true);
  });

  // Identical to a server whose agent never reported an inventory: absent is
  // UNKNOWN, and an unknown must never be the thing that stops a deploy.
  it("fails open when no node has reported a GPU figure", () => {
    expect(clusterOptions(cluster(), "env", "llm", inv, modelCard()).at(0)?.eligible).toBe(true);
    expect(clusterOptions(cluster(0), "env", "llm", inv, modelCard()).at(0)?.eligible).toBe(true);
  });

  it("offers every cluster before a model is chosen", () => {
    expect(clusterOptions(cluster(24 * GB), "env", "llm", inv).at(0)?.eligible).toBe(true);
  });
});

// The step's props were never the thing that was wrong — the arguments were.
// `model` reached serverOptions and never reached clusterOptions, and no suite
// could see it because the wiring lived in JSX and these tests have no DOM.
// Assembling the props here is what makes that wiring assertable.
describe("the target step is handed the model, for both kinds of target", () => {
  const projects: WizardProject[] = [
    {
      id: "prj",
      name: "Project",
      environments: [
        {
          id: "env",
          name: "production",
          servers: [
            {
              id: "small",
              name: "gpu-l4-01",
              type: "gpu",
              status: "running",
              gpu: { vendor: "nvidia", count: 1, vramBytesPerGpu: 24 * GB },
            },
          ],
        },
      ],
    },
  ];
  const clusters = [
    { id: "cl", name: "prod", environmentId: "env", maxVramBytesPerGpu: 24 * GB },
  ];
  const inv = buildInventory(projects, clusters, []);
  const at = (model?: ModelCard) =>
    targetChoices({
      projects,
      clusters,
      inventory: inv,
      kind: "llm",
      model,
      projectId: "prj",
      environmentId: "env",
    });

  it("filters the servers by the chosen model", () => {
    const [server] = at(modelCard()).servers;
    expect(server.eligible).toBe(false);
    expect(server.reason).toContain("188 GB");
  });

  it("filters the clusters by the same model", () => {
    const [cluster] = at(modelCard()).clusters;
    expect(cluster.eligible).toBe(false);
    expect(cluster.reason).toContain("188 GB");
  });

  it("offers both when the model fits", () => {
    const fits = at(modelCard({ vramBytesRequired: 20 * GB, vramText: "~20 GB" }));
    expect(fits.servers.every((s) => s.eligible)).toBe(true);
    expect(fits.clusters.every((c) => c.eligible)).toBe(true);
    expect(fits.deadEnd).toBeNull();
  });

  it("offers the chosen project's environments to pick from", () => {
    expect(at().environments.map((e) => e.id)).toEqual(["env"]);
  });

  // Every row carrying its own reason still leaves "so what do I do"
  // unanswered, and the Continue button's own message was "Pick a server or a
  // cluster" — advice for a screen where something is pickable.
  it("says what to do when nothing here can run the model", () => {
    const dead = at(modelCard()).deadEnd;
    expect(dead).toContain("188 GB");
    expect(dead).toContain("quantized");
  });

  it("blames the environment, not the model, when the model is not the cause", () => {
    const dead = targetChoices({
      projects,
      clusters: [],
      inventory: inv,
      kind: "postgres",
      projectId: "prj",
      environmentId: "env",
    }).deadEnd;
    expect(dead).toContain("Attach a compatible server");
  });

  it("says nothing at all when the environment has nothing attached", () => {
    const empty = targetChoices({
      projects: [
        {
          id: "prj",
          name: "Project",
          environments: [{ id: "env", name: "production", servers: [] }],
        },
      ],
      clusters: [],
      inventory: inv,
      kind: "llm",
      model: modelCard(),
      projectId: "prj",
      environmentId: "env",
    });
    expect(empty.servers).toEqual([]);
    expect(empty.deadEnd).toBeNull();
  });
});
