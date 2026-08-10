import { describe, expect, it } from "vitest";
import {
  CLUSTER_EXCLUDED_KINDS,
  CONNECTABLE_SERVER_TYPES,
  RESOURCE_CATEGORIES,
  RESOURCE_CATEGORY_CATALOG,
} from "@/lib/server-catalog.generated";
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
    vramText: "~189 GB",
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
    // Every host type an operator can connect EXCEPT a GPU one, rather than the
    // three that were typed here: the sentence has to be about hardware however
    // much other hardware is already in the fleet, and a type added to the
    // catalog is precisely the one that would have been left out of a hand list.
    const inv = buildInventory(projectWith(CONNECTABLE_SERVER_TYPES.filter((t) => t !== "gpu")));
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
      CLUSTER_EXCLUDED_KINDS
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
      CLUSTER_EXCLUDED_KINDS
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
      "This model needs about 189 GB of VRAM; this server's GPU has 24 GB."
    );
  });

  // Four 80 GB cards is 320 GB of VRAM and still cannot run a 189 GB model:
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
      vramText: "~21.4 GB",
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

// A model endpoint cannot run inside a cluster on this control plane: nothing
// renders a cluster-targeted one, and its provisioning is server-scoped, so the
// create used to die on a foreign-key violation surfaced as a 500. The wizard's
// answer is the refusal it already had for the stateful engines, one screen
// earlier — and no VRAM comparison, which is why the cluster listing stopped
// publishing a GPU figure at all.
describe("a model endpoint is refused for the cluster itself, at any size", () => {
  const clusters = [{ id: "cl", name: "prod", environmentId: "env" }];
  const inv = buildInventory([], clusters, CLUSTER_EXCLUDED_KINDS);

  it("refuses the cluster and names where a model endpoint does run", () => {
    const [opt] = clusterOptions(clusters, "env", "llm", inv);
    expect(opt.eligible).toBe(false);
    expect(opt.reason).toContain("GPU server");
    expect(opt.reason).toContain("rather than inside a cluster");
  });

  // The stateful engines and the model endpoint are excluded for unrelated
  // reasons, and saying the database one about an inference server tells an
  // operator their model is at risk of data loss — which is false, and leaves
  // the real reason unsaid.
  it("does not tell a model endpoint it is a database", () => {
    const [llm] = clusterOptions(clusters, "env", "llm", inv);
    expect(llm.reason).not.toContain("data");
    const [pg] = clusterOptions(clusters, "env", "postgres", inv);
    expect(pg.reason).toContain("keeps its data on one host");
  });

  // Not "this model is too big". A cluster row can no longer be handed a model
  // at all, so nothing here can produce a size sentence — and a size sentence
  // would be the wrong advice anyway, since no smaller model would help.
  it("says nothing about VRAM", () => {
    expect(clusterOptions(clusters, "env", "llm", inv)[0].reason).not.toContain("VRAM");
  });

  // The kind exclusion has to reach step ONE as well, and it does by a route
  // worth pinning: kindAvailability answers "available" for anything a cluster
  // can host BEFORE it reaches the GPU sentence, so an org with a cluster and no
  // GPU server would have been offered Model endpoint with no hardware sentence
  // and no way to deploy it.
  it("still explains the missing GPU to an org whose only target is a cluster", () => {
    const verdict = kindAvailability("llm", inv);
    expect(verdict.available).toBe(false);
    expect(verdict.reason).toContain("GPU");
    expect(verdict.action?.href).toBe("/dashboard/servers");
  });
});

// The step's props were never the thing that was wrong — the arguments were.
// `model` reached serverOptions and never reached clusterOptions, and no suite
// could see it because the wiring lived in JSX and these tests have no DOM.
// Assembling the props here is what makes that wiring assertable.
describe("the target step is handed the model, and each target answers in its own terms", () => {
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
  const clusters = [{ id: "cl", name: "prod", environmentId: "env" }];
  const inv = buildInventory(projects, clusters, ["llm"]);
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
    expect(server.reason).toContain("189 GB");
  });

  // The cluster row is refused whatever the model is, and for the other reason:
  // it is not a place a model endpoint runs.
  it("refuses the cluster for the kind, never for the model", () => {
    const [cluster] = at(modelCard()).clusters;
    expect(cluster.eligible).toBe(false);
    expect(cluster.reason).toContain("rather than inside a cluster");
    expect(cluster.reason).not.toContain("GB");
  });

  it("offers the server once the model fits, with the cluster still refused", () => {
    const fits = at(modelCard({ vramBytesRequired: 20 * GB, vramText: "~20.0 GB" }));
    expect(fits.servers.every((s) => s.eligible)).toBe(true);
    expect(fits.clusters.every((c) => c.eligible)).toBe(false);
    // Something IS pickable, so there is no dead end to explain.
    expect(fits.deadEnd).toBeNull();
  });

  it("offers the chosen project's environments to pick from", () => {
    expect(at().environments.map((e) => e.id)).toEqual(["env"]);
  });

  // Every row carrying its own reason still leaves "so what do I do"
  // unanswered, and the Continue button's own message was "Pick a server or a
  // cluster" — advice for a screen where something is pickable. The cluster in
  // this environment is refused too, for a reason no smaller model would fix,
  // and that must not change the advice the GPU rows earned.
  it("says what to do when nothing here can run the model", () => {
    const dead = at(modelCard()).deadEnd;
    expect(dead).toContain("189 GB");
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

/**
 * The engines a control plane has enabled are its own fact, and until it
 * published them (SIGMA-268) the wizard could not know it: the cards come from
 * the generated catalog, which is every engine this codebase can provision, and
 * a deployment running CP_DB_ENGINES=postgres refuses the rest at create — a
 * 422 the operator meets after the dialog has closed, with the project, the
 * environment and the server they picked in it.
 */
describe("engines the control plane has enabled", () => {
  const withDatabaseServer = projectWith(["database", "storage"]);

  it("refuses a kind whose engine this control plane does not have, whatever hardware is connected", () => {
    const inv = buildInventory(withDatabaseServer, [], [], {
      dbEngines: ["postgres"],
      s3Engines: ["minio"],
    });

    expect(kindAvailability("postgres", inv).available).toBe(true);

    const mongo = kindAvailability("mongodb", inv);
    expect(mongo.available).toBe(false);
    // And the sentence must not be the hardware one: a Database server IS
    // connected here, so "connect a Database server" would send the operator
    // after a machine that changes nothing.
    expect(mongo.reason).toContain("does not have");
    expect(mongo.reason).toContain("CP_DB_ENGINES");
    expect(mongo.action).toBeUndefined();
  });

  it("keeps object storage while any of its engines is enabled", () => {
    const inv = buildInventory(withDatabaseServer, [], [], {
      dbEngines: ["postgres"],
      s3Engines: ["minio"],
    });
    expect(kindAvailability("s3", inv).available).toBe(true);
  });

  it("does not gate kinds that have no engine setting behind them", () => {
    const inv = buildInventory(projectWith(["general", "gpu"]), [], [], {
      dbEngines: ["postgres"],
      s3Engines: ["minio"],
    });
    expect(kindAvailability("app", inv).available).toBe(true);
    expect(kindAvailability("llm", inv).available).toBe(true);
  });

  it("assumes the whole catalog when nothing has published a list", () => {
    // The pre-SIGMA-268 behaviour, and the behaviour on a failed read: the
    // create call is still the authority, and narrowing on an answer we never
    // got would hide engines a working deployment has.
    const inv = buildInventory(withDatabaseServer);
    for (const kind of RESOURCE_CATEGORY_CATALOG.database.kinds) {
      expect(kindAvailability(kind, inv).available).toBe(true);
    }
  });

  it("keeps the category open while one engine inside it survives", () => {
    const inv = buildInventory(withDatabaseServer, [], [], {
      dbEngines: ["postgres"],
      s3Engines: ["minio"],
    });
    expect(categoryAvailability("database", inv).available).toBe(true);
  });
});
