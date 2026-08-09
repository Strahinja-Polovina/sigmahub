import type {
  Org,
  Project,
  Environment,
  Cluster,
  Server,
  Resource,
  Member,
} from "./types";
import type { HostFacts } from "@/lib/server-compat";

export const orgs: Org[] = [
  { id: "org_acme", name: "Acme Cloud", slug: "acme", plan: "cloud", memberCount: 4, serverCount: 13 },
  { id: "org_northwind", name: "Northwind", slug: "northwind", plan: "free", memberCount: 2, serverCount: 1 },
];

export const projects: Project[] = [
  { id: "proj_webshop", orgId: "org_acme", name: "Webshop", slug: "webshop", description: "Customer-facing storefront", environmentIds: ["env_webshop_prod", "env_webshop_staging"] },
  { id: "proj_api", orgId: "org_acme", name: "API Platform", slug: "api-platform", description: "Core REST + gRPC services", environmentIds: ["env_api_prod", "env_api_dev"] },
  { id: "proj_mllab", orgId: "org_acme", name: "ML Lab", slug: "ml-lab", description: "LLM inference & experiments", environmentIds: ["env_mllab_prod", "env_mllab_staging"] },
  { id: "proj_site", orgId: "org_northwind", name: "Marketing Site", slug: "site", description: "Static site + blog", environmentIds: ["env_site_prod"] },
];

export const environments: Environment[] = [
  { id: "env_webshop_prod", projectId: "proj_webshop", name: "production", serverIds: ["srv_gen_1", "srv_db_1"] },
  { id: "env_webshop_staging", projectId: "proj_webshop", name: "staging", serverIds: ["srv_gen_2"] },
  // The cluster lives here (see `clusters`). Its NODES are deliberately not
  // attached to the environment: a node receives work through the cluster's
  // control plane, so offering it as a deploy target of its own would put a row
  // in the target picker whose only possible answer is "no".
  { id: "env_api_prod", projectId: "proj_api", name: "production", serverIds: ["srv_gen_1", "srv_db_1", "srv_store_1"] },
  { id: "env_api_dev", projectId: "proj_api", name: "dev", serverIds: ["srv_gen_2"] },
  // Two cards of different sizes in ONE environment, which is what makes the
  // VRAM fit check legible: the target step shows both, refuses one of them for
  // the chosen model, and says which number it failed on (SIGMA-214).
  { id: "env_mllab_prod", projectId: "proj_mllab", name: "production", serverIds: ["srv_gpu_1", "srv_gpu_3"] },
  { id: "env_mllab_staging", projectId: "proj_mllab", name: "staging", serverIds: ["srv_gpu_4"] },
  { id: "env_site_prod", projectId: "proj_site", name: "production", serverIds: ["srv_nw_1"] },
];

// What every Linux host in this fleet reports identically, so each fixture below
// states only the part that makes it that machine. These are the two distros the
// catalog accepts, spelled with the ids the agent reads out of /etc/os-release —
// a demo host filed under a distro the catalog does not list would be refused by
// the gate, which is a different demo than the one intended.
const UBUNTU_24_04 = {
  os: "linux",
  arch: "amd64",
  kernel: "6.8.0-51-generic",
  distro: "ubuntu-24.04",
  distroName: "Ubuntu 24.04.1 LTS",
  diskPath: "/",
  dockerAvailable: true,
  dockerVersion: "27.3.1",
} satisfies HostFacts;

const DEBIAN_12 = {
  os: "linux",
  arch: "amd64",
  kernel: "6.1.0-28-amd64",
  distro: "debian-12",
  distroName: "Debian GNU/Linux 12 (bookworm)",
  diskPath: "/",
  dockerAvailable: true,
  dockerVersion: "27.3.1",
} satisfies HostFacts;

// Disk and VRAM figures are ENUMERATED sizes — what the kernel and nvidia-smi
// actually report — never the number on the invoice. A "2 TB" NVMe is
// 2000398934016 bytes and a "24 GB" A10G is 22731 MiB, and the whole argument of
// both the disk floor and the VRAM fit check is that the rounded figure approves
// hosts and models that then run out of room. A fixture written from marketing
// numbers would demo a check that never says no.
export const servers: Server[] = [
  {
    id: "srv_gen_1", orgId: "org_acme", name: "hel-general-01", type: "general",
    provider: "Hetzner", region: "eu-central · HEL1", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.11", meshIp: "10.8.0.11", cpu: 8, memGb: 32,
    connectedAt: "2027-01-12", environmentIds: ["env_webshop_prod", "env_api_prod"],
    resourceCount: 2, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "hel-general-01", numCpu: 8, memTotalMb: 32_768,
      diskTotalBytes: 480_103_981_056, diskFreeBytes: 291_447_144_448,
    },
  },
  {
    id: "srv_gen_2", orgId: "org_acme", name: "fsn-general-02", type: "general",
    provider: "Hetzner", region: "eu-central · FSN1", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.12", meshIp: "10.8.0.12", cpu: 4, memGb: 16,
    connectedAt: "2027-01-14", environmentIds: ["env_webshop_staging", "env_api_dev"],
    resourceCount: 1, byoVpn: true,
    facts: {
      ...UBUNTU_24_04, hostname: "fsn-general-02", numCpu: 4, memTotalMb: 16_384,
      diskTotalBytes: 512_110_190_592, diskFreeBytes: 388_214_489_088,
    },
  },
  {
    id: "srv_db_1", orgId: "org_acme", name: "hel-db-01", type: "database",
    provider: "Hetzner", region: "eu-central · HEL1", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.21", meshIp: "10.8.0.21", cpu: 8, memGb: 64,
    connectedAt: "2027-01-12", environmentIds: ["env_webshop_prod", "env_api_prod"],
    resourceCount: 2, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "hel-db-01", numCpu: 8, memTotalMb: 65_536,
      diskTotalBytes: 2_000_398_934_016, diskFreeBytes: 1_204_317_847_552,
    },
  },
  {
    id: "srv_store_1", orgId: "org_acme", name: "gra-store-01", type: "storage",
    provider: "OVH", region: "eu-west · GRA", status: "running",
    agentVersion: "1.4.1", ip: "203.0.113.31", meshIp: "10.8.0.31", cpu: 4, memGb: 16,
    connectedAt: "2027-01-20", environmentIds: ["env_api_prod"],
    resourceCount: 1, byoVpn: false,
    facts: {
      ...DEBIAN_12, hostname: "gra-store-01", numCpu: 4, memTotalMb: 16_384,
      diskTotalBytes: 8_001_563_222_016, diskFreeBytes: 5_412_336_791_552,
    },
  },
  // The demo's original GPU host. Its facts include the card's memory because
  // that is what the model step's VRAM filter compares against (SIGMA-214): with
  // no inventory reported the fit check correctly does nothing, and a demo where
  // it does nothing hides the feature from everyone evaluating it. 42949672960
  // bytes is 40960 MiB, what nvidia-smi reports for a 40 GB A100 — a real
  // reading, so the demo's arithmetic is the arithmetic a customer's fleet
  // produces.
  {
    id: "srv_gpu_1", orgId: "org_acme", name: "gpu-a100-01", type: "gpu",
    provider: "BYO · bare metal", region: "on-prem · RS", status: "degraded",
    agentVersion: "1.4.2", ip: "203.0.113.41", meshIp: "10.8.0.41", cpu: 16, memGb: 128,
    connectedAt: "2027-02-02", environmentIds: ["env_mllab_prod"], resourceCount: 1, byoVpn: true,
    facts: {
      ...UBUNTU_24_04, hostname: "gpu-a100-01", numCpu: 16, memTotalMb: 131_072,
      diskTotalBytes: 2_000_398_934_016, diskFreeBytes: 1_240_517_869_568,
      gpu: {
        vendor: "nvidia", model: "NVIDIA A100-PCIE-40GB", count: 1,
        vramBytesPerGpu: 42_949_672_960, vramBytesTotal: 42_949_672_960,
        driverVersion: "550.54.15",
        cards: [{ index: 0, model: "NVIDIA A100-PCIE-40GB", vramBytes: 42_949_672_960 }],
      },
    },
  },
  // The big card, and the reason there is more than one. A fleet where every GPU
  // is the same size can only ever demonstrate "it fits" or "it doesn't" — never
  // the sentence the fit check is for, which is that THIS model fits THAT host
  // and not the one beside it. 85520809984 bytes is 81559 MiB, what nvidia-smi
  // reports for an 80 GB H100: not 80, and not 81920 either.
  {
    id: "srv_gpu_3", orgId: "org_acme", name: "gpu-h100-01", type: "gpu",
    provider: "BYO · bare metal", region: "on-prem · RS", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.42", meshIp: "10.8.0.42", cpu: 32, memGb: 256,
    connectedAt: "2027-02-24", environmentIds: ["env_mllab_prod"], resourceCount: 1, byoVpn: true,
    facts: {
      ...UBUNTU_24_04, hostname: "gpu-h100-01", numCpu: 32, memTotalMb: 262_144,
      diskTotalBytes: 4_000_787_030_016, diskFreeBytes: 2_914_239_381_504,
      gpu: {
        vendor: "nvidia", model: "NVIDIA H100 80GB HBM3", count: 1,
        vramBytesPerGpu: 85_520_809_984, vramBytesTotal: 85_520_809_984,
        driverVersion: "565.57.01",
        cards: [{ index: 0, model: "NVIDIA H100 80GB HBM3", vramBytes: 85_520_809_984 }],
      },
    },
  },
  // The small card, and the one that says no. 23835181056 bytes is 22731 MiB —
  // an A10G sold as "24 GB", which is 165 MiB short of what a model sized against
  // the marketing figure would need. That gap is the entire argument for
  // comparing enumerated bytes, and this host is where a demo can see it.
  {
    id: "srv_gpu_4", orgId: "org_acme", name: "gpu-a10g-01", type: "gpu",
    provider: "AWS", region: "eu-central-1 · FRA", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.43", meshIp: "10.8.0.43", cpu: 4, memGb: 16,
    connectedAt: "2027-02-26", environmentIds: ["env_mllab_staging"], resourceCount: 1, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "gpu-a10g-01", numCpu: 4, memTotalMb: 16_384,
      diskTotalBytes: 250_059_350_016, diskFreeBytes: 191_203_860_480,
      gpu: {
        vendor: "nvidia", model: "NVIDIA A10G", count: 1,
        vramBytesPerGpu: 23_835_181_056, vramBytesTotal: 23_835_181_056,
        driverVersion: "550.127.05",
        cards: [{ index: 0, model: "NVIDIA A10G", vramBytes: 23_835_181_056 }],
      },
    },
  },
  // Connected and waiting for its first check-in, which is why it reports NO
  // facts, no agent version and no mesh address: those all arrive together when
  // the agent first speaks. Writing them here would seed a host that is
  // simultaneously provisioning and fully known, and the demo's "simulate the
  // agent checking in" button would have nothing left to do.
  {
    id: "srv_gen_3", orgId: "org_acme", name: "ash-general-03", type: "general",
    provider: "Hetzner", region: "us-east · ASH", status: "provisioning",
    agentVersion: "", ip: "203.0.113.13", meshIp: "", cpu: 0, memGb: 0,
    connectedAt: "2027-03-01", environmentIds: [], resourceCount: 0, byoVpn: false,
  },
  // The misfiled host: an ordinary box someone connected as a GPU server. Its
  // STATUS is not written here — the seed runs the real compatibility gate over
  // these facts, so the demo shows the same verdict, in the same words, that a
  // real machine would get (SIGMA-203). It exists because the incompatible state
  // has to be visible in a demo, and nobody demoing owns a GPU box with no GPU
  // in it.
  //
  // `gpu: { vendor: "", count: 0 }` is the load-bearing part: it is the agent
  // saying it LOOKED and found nothing, which is a fact the gate must act on.
  // Omitting the key would mean "nobody looked", and the gate would — correctly —
  // pass the host.
  {
    id: "srv_gpu_2", orgId: "org_acme", name: "fsn-gpu-02", type: "gpu",
    provider: "Hetzner", region: "eu-central · FSN1", status: "provisioning",
    agentVersion: "1.4.2", ip: "203.0.113.44", meshIp: "10.8.0.44", cpu: 4, memGb: 16,
    connectedAt: "2027-03-04", environmentIds: [], resourceCount: 0, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "fsn-gpu-02", numCpu: 4, memTotalMb: 16_384,
      diskTotalBytes: 480_103_981_056, diskFreeBytes: 402_255_675_392,
      gpu: { vendor: "", count: 0 },
    },
  },
  // The cluster's three machines. They are filed as `k8s` — the catalog's
  // "Cluster node" type, which hosts nothing directly and bills at its own
  // weight — because that is what they are: work reaches them through the
  // cluster's control plane. Filing them as General would have the wizard offer
  // three servers whose real answer to "can you host this" is the cluster's.
  {
    id: "srv_k8s_1", orgId: "org_acme", name: "hel-k8s-01", type: "k8s",
    provider: "Hetzner", region: "eu-central · HEL1", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.51", meshIp: "10.8.0.51", cpu: 8, memGb: 32,
    connectedAt: "2027-03-05", environmentIds: [], resourceCount: 0, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "hel-k8s-01", numCpu: 8, memTotalMb: 32_768,
      diskTotalBytes: 480_103_981_056, diskFreeBytes: 402_984_697_856,
    },
  },
  {
    id: "srv_k8s_2", orgId: "org_acme", name: "hel-k8s-02", type: "k8s",
    provider: "Hetzner", region: "eu-central · HEL1", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.52", meshIp: "10.8.0.52", cpu: 8, memGb: 32,
    connectedAt: "2027-03-05", environmentIds: [], resourceCount: 0, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "hel-k8s-02", numCpu: 8, memTotalMb: 32_768,
      diskTotalBytes: 480_103_981_056, diskFreeBytes: 421_318_692_864,
    },
  },
  // The third node, in the other datacentre — the cluster spans the mesh rather
  // than a rack, which is the point of running it over WireGuard.
  {
    id: "srv_k8s_3", orgId: "org_acme", name: "fsn-k8s-03", type: "k8s",
    provider: "Hetzner", region: "eu-central · FSN1", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.53", meshIp: "10.8.0.53", cpu: 8, memGb: 32,
    connectedAt: "2027-03-06", environmentIds: [], resourceCount: 0, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "fsn-k8s-03", numCpu: 8, memTotalMb: 32_768,
      diskTotalBytes: 480_103_981_056, diskFreeBytes: 448_921_890_816,
    },
  },
  // A teardown in flight (SIGMA-204). `status: "running"` is the host's own
  // condition and the seed overrides it with `decommissioning`, for the same
  // reason the gate overrides a misfiled host's status: the machine is fine, and
  // what is true about it is a decision the operator made.
  //
  // TWO MINUTES, and relative to the seed run rather than a calendar date. The
  // dialog decides what to offer by comparing this against the control plane's
  // ten-minute window, so a fixed date would pick that answer for us: any date in
  // the past makes every demo, forever, show a teardown that has already timed
  // out, and the graceful half — the state a freshly-clicked Disconnect actually
  // produces — could never be seen. Two minutes in leaves eight of the ten, so a
  // fresh demo opens on a teardown that is still working and the operator can
  // drive it either way from there ("Simulate: agent acknowledged" finishes it,
  // "Simulate: timeout" ages it into the Force disconnect path). A database
  // seeded and then left for a week crosses the window on its own, which is not a
  // drift: it is the row reaching the state the product would have put it in, and
  // the dialog says exactly that.
  {
    id: "srv_gen_4", orgId: "org_acme", name: "fsn-general-04", type: "general",
    provider: "Hetzner", region: "eu-central · FSN1", status: "running",
    agentVersion: "1.4.2", ip: "203.0.113.14", meshIp: "10.8.0.14", cpu: 4, memGb: 16,
    connectedAt: "2027-01-30", environmentIds: [], resourceCount: 0, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "fsn-general-04", numCpu: 4, memTotalMb: 16_384,
      diskTotalBytes: 512_110_190_592, diskFreeBytes: 495_838_461_952,
    },
    decommission: { startedMinutesAgo: 2, purgeVolumes: false },
  },
  {
    id: "srv_nw_1", orgId: "org_northwind", name: "ams-general-01", type: "general",
    provider: "DigitalOcean", region: "eu · AMS3", status: "running",
    agentVersion: "1.4.2", ip: "198.51.100.11", meshIp: "10.9.0.11", cpu: 2, memGb: 4,
    connectedAt: "2027-02-10", environmentIds: ["env_site_prod"], resourceCount: 2, byoVpn: false,
    facts: {
      ...UBUNTU_24_04, hostname: "ams-general-01", numCpu: 2, memTotalMb: 4_096,
      diskTotalBytes: 80_026_361_856, diskFreeBytes: 41_203_884_032,
    },
  },
];

/**
 * The org's one Kubernetes cluster.
 *
 * It sits in an environment that already has servers and resources, because that
 * mixed estate IS the product's position on clusters: the app runs in the
 * cluster and reaches its database over the mesh, on the database's own host,
 * where a rescheduling event cannot separate it from its data. api-gateway below
 * is the cluster workload; api-postgres and assets are its neighbours outside.
 *
 * It reads READY, which is not a choice made here — it follows from three
 * running hosts that joined days ago, through the demo's own derivation. The
 * more interesting states are not seedable and do not need to be: a node reports
 * `error` when its HOST is unreachable, incompatible or being decommissioned
 * (demoNodeReport), so a fixture that wrote a broken node over a healthy host
 * would be corrected on the first render. They are reached the way the product
 * reaches them — drive a node's host from the servers page and watch the cluster
 * go degraded — and seeding `ready` is what keeps that walk available, since a
 * `provisioning` cluster is an ineligible deploy target and would close the
 * wizard path this fixture exists to open.
 *
 * Every node here is also a server the create dialog would have ACCEPTED:
 * controlPlaneRefusal turns away a host that is not running, so a cluster seeded
 * over one would be a state the product refuses to produce.
 */
export const clusters: Cluster[] = [
  {
    id: "cls_api_prod",
    orgId: "org_acme",
    environmentId: "env_api_prod",
    name: "api-prod",
    createdBy: "Mila Jovanović",
    createdDaysAgo: 9,
    nodes: [
      { serverId: "srv_k8s_1", role: "control-plane", joinedDaysAgo: 9 },
      { serverId: "srv_k8s_2", role: "worker", joinedDaysAgo: 9 },
      { serverId: "srv_k8s_3", role: "worker", joinedDaysAgo: 7 },
    ],
  },
];

export const resources: Resource[] = [
  { id: "res_web", projectId: "proj_webshop", environmentId: "env_webshop_prod", serverId: "srv_gen_1", name: "storefront", kind: "app", status: "running", lastDeployAt: "2027-03-02T09:12:00Z", repo: "acme/storefront", domain: "shop.acme.com", version: "v128" },
  { id: "res_webdb", projectId: "proj_webshop", environmentId: "env_webshop_prod", serverId: "srv_db_1", name: "shop-postgres", kind: "postgres", status: "running", lastDeployAt: "2027-02-20T10:00:00Z", version: "16.2" },
  { id: "res_webcache", projectId: "proj_webshop", environmentId: "env_webshop_prod", serverId: "srv_gen_1", name: "shop-redis", kind: "redis", status: "running", lastDeployAt: "2027-02-20T10:02:00Z", version: "7.4" },
  // The cluster workload: no server of its own, because the scheduler picks the
  // node. Its neighbours in this environment are a Postgres and a bucket on
  // their own hosts — the kinds the control plane refuses to run inside a
  // cluster — so the rule and its exception are both on one screen.
  { id: "res_api", projectId: "proj_api", environmentId: "env_api_prod", serverId: null, clusterId: "cls_api_prod", name: "api-gateway", kind: "app", status: "running", lastDeployAt: "2027-03-03T14:30:00Z", repo: "acme/api", domain: "api.acme.com", version: "v452" },
  { id: "res_apidb", projectId: "proj_api", environmentId: "env_api_prod", serverId: "srv_db_1", name: "api-postgres", kind: "postgres", status: "running", lastDeployAt: "2027-02-28T08:00:00Z", version: "16.2" },
  { id: "res_apis3", projectId: "proj_api", environmentId: "env_api_prod", serverId: "srv_store_1", name: "assets", kind: "s3", status: "running", lastDeployAt: "2027-02-25T12:00:00Z" },
  { id: "res_worker", projectId: "proj_api", environmentId: "env_api_dev", serverId: "srv_gen_2", name: "worker", kind: "app", status: "degraded", lastDeployAt: "2027-03-01T16:45:00Z", repo: "acme/api", version: "v451" },
  // Every model endpoint here is one the product would have LET you create: the
  // estimate in mock/models.ts fits the card its host reports. The demo used to
  // run a 70B checkpoint on a 40 GB A100 — 188 GB of weights on a card that
  // holds 42 — so the fleet contradicted the fit check on the screen next to it,
  // and the first thing a prospective user would learn is that the check is
  // decorative.
  { id: "res_llm", projectId: "proj_mllab", environmentId: "env_mllab_prod", serverId: "srv_gpu_1", name: "llama-3-8b", kind: "llm", status: "degraded", lastDeployAt: "2027-03-01T11:00:00Z", version: "vllm 0.6" },
  { id: "res_llm_awq", projectId: "proj_mllab", environmentId: "env_mllab_prod", serverId: "srv_gpu_3", name: "llama-3-70b-awq", kind: "llm", status: "running", lastDeployAt: "2027-03-04T08:20:00Z", version: "vllm 0.6" },
  { id: "res_llm_qwen", projectId: "proj_mllab", environmentId: "env_mllab_staging", serverId: "srv_gpu_4", name: "qwen2-5-7b", kind: "llm", status: "running", lastDeployAt: "2027-03-04T09:05:00Z", version: "vllm 0.6" },
  { id: "res_site", projectId: "proj_site", environmentId: "env_site_prod", serverId: "srv_nw_1", name: "marketing", kind: "app", status: "running", lastDeployAt: "2027-03-02T07:00:00Z", repo: "nw/site", domain: "northwind.com", version: "v33" },
  { id: "res_siteblog", projectId: "proj_site", environmentId: "env_site_prod", serverId: "srv_nw_1", name: "blog-mongo", kind: "mongodb", status: "running", lastDeployAt: "2027-02-15T09:00:00Z", version: "7.0" },
];

export const members: Member[] = [
  { id: "u1", name: "Strahinja Polovina", email: "strahinja@sigmajunction.com", role: "Org Admin" },
  { id: "u2", name: "Mila Jovanović", email: "mila@acme.com", role: "Project Admin" },
  { id: "u3", name: "Nikola Petrović", email: "nikola@acme.com", role: "Developer" },
  { id: "u4", name: "Ana Kovač", email: "ana@acme.com", role: "Developer" },
];
