import type {
  Org,
  Project,
  Environment,
  Server,
  Resource,
  Member,
} from "./types";

export const orgs: Org[] = [
  { id: "org_acme", name: "Acme Cloud", slug: "acme", plan: "cloud", memberCount: 4, serverCount: 6 },
  { id: "org_northwind", name: "Northwind", slug: "northwind", plan: "free", memberCount: 2, serverCount: 1 },
];

export const projects: Project[] = [
  { id: "proj_webshop", orgId: "org_acme", name: "Webshop", slug: "webshop", description: "Customer-facing storefront", environmentIds: ["env_webshop_prod", "env_webshop_staging"] },
  { id: "proj_api", orgId: "org_acme", name: "API Platform", slug: "api-platform", description: "Core REST + gRPC services", environmentIds: ["env_api_prod", "env_api_dev"] },
  { id: "proj_mllab", orgId: "org_acme", name: "ML Lab", slug: "ml-lab", description: "LLM inference & experiments", environmentIds: ["env_mllab_prod"] },
  { id: "proj_site", orgId: "org_northwind", name: "Marketing Site", slug: "site", description: "Static site + blog", environmentIds: ["env_site_prod"] },
];

export const environments: Environment[] = [
  { id: "env_webshop_prod", projectId: "proj_webshop", name: "production", serverIds: ["srv_gen_1", "srv_db_1"] },
  { id: "env_webshop_staging", projectId: "proj_webshop", name: "staging", serverIds: ["srv_gen_2"] },
  { id: "env_api_prod", projectId: "proj_api", name: "production", serverIds: ["srv_gen_1", "srv_db_1", "srv_store_1"] },
  { id: "env_api_dev", projectId: "proj_api", name: "dev", serverIds: ["srv_gen_2"] },
  { id: "env_mllab_prod", projectId: "proj_mllab", name: "production", serverIds: ["srv_gpu_1"] },
  { id: "env_site_prod", projectId: "proj_site", name: "production", serverIds: ["srv_nw_1"] },
];

export const servers: Server[] = [
  { id: "srv_gen_1", orgId: "org_acme", name: "hel-general-01", type: "general", provider: "Hetzner", region: "eu-central · HEL1", status: "running", agentVersion: "1.4.2", ip: "10.8.0.11", cpu: 8, memGb: 32, connectedAt: "2027-01-12", environmentIds: ["env_webshop_prod", "env_api_prod"], resourceCount: 4, byoVpn: false },
  { id: "srv_gen_2", orgId: "org_acme", name: "fsn-general-02", type: "general", provider: "Hetzner", region: "eu-central · FSN1", status: "running", agentVersion: "1.4.2", ip: "10.8.0.12", cpu: 4, memGb: 16, connectedAt: "2027-01-14", environmentIds: ["env_webshop_staging", "env_api_dev"], resourceCount: 3, byoVpn: true },
  { id: "srv_db_1", orgId: "org_acme", name: "hel-db-01", type: "database", provider: "Hetzner", region: "eu-central · HEL1", status: "running", agentVersion: "1.4.2", ip: "10.8.0.21", cpu: 8, memGb: 64, connectedAt: "2027-01-12", environmentIds: ["env_webshop_prod", "env_api_prod"], resourceCount: 3, byoVpn: false },
  { id: "srv_store_1", orgId: "org_acme", name: "gra-store-01", type: "storage", provider: "OVH", region: "eu-west · GRA", status: "running", agentVersion: "1.4.1", ip: "10.8.0.31", cpu: 4, memGb: 16, connectedAt: "2027-01-20", environmentIds: ["env_api_prod"], resourceCount: 1, byoVpn: false },
  { id: "srv_gpu_1", orgId: "org_acme", name: "gpu-a100-01", type: "gpu", provider: "BYO · bare metal", region: "on-prem · RS", status: "degraded", agentVersion: "1.4.2", ip: "10.8.0.41", cpu: 16, memGb: 128, connectedAt: "2027-02-02", environmentIds: ["env_mllab_prod"], resourceCount: 1, byoVpn: true },
  { id: "srv_gen_3", orgId: "org_acme", name: "ash-general-03", type: "general", provider: "Hetzner", region: "us-east · ASH", status: "provisioning", agentVersion: "1.4.2", ip: "10.8.0.13", cpu: 4, memGb: 16, connectedAt: "2027-03-01", environmentIds: [], resourceCount: 0, byoVpn: false },
  { id: "srv_nw_1", orgId: "org_northwind", name: "ams-general-01", type: "general", provider: "DigitalOcean", region: "eu · AMS3", status: "running", agentVersion: "1.4.2", ip: "10.9.0.11", cpu: 2, memGb: 4, connectedAt: "2027-02-10", environmentIds: ["env_site_prod"], resourceCount: 2, byoVpn: false },
];

export const resources: Resource[] = [
  { id: "res_web", projectId: "proj_webshop", environmentId: "env_webshop_prod", serverId: "srv_gen_1", name: "storefront", kind: "app", status: "running", lastDeployAt: "2027-03-02T09:12:00Z", repo: "acme/storefront", domain: "shop.acme.com", version: "v128" },
  { id: "res_webdb", projectId: "proj_webshop", environmentId: "env_webshop_prod", serverId: "srv_db_1", name: "shop-postgres", kind: "postgres", status: "running", lastDeployAt: "2027-02-20T10:00:00Z", version: "16.2" },
  { id: "res_webcache", projectId: "proj_webshop", environmentId: "env_webshop_prod", serverId: "srv_gen_1", name: "shop-redis", kind: "redis", status: "running", lastDeployAt: "2027-02-20T10:02:00Z", version: "7.4" },
  { id: "res_api", projectId: "proj_api", environmentId: "env_api_prod", serverId: "srv_gen_1", name: "api-gateway", kind: "app", status: "running", lastDeployAt: "2027-03-03T14:30:00Z", repo: "acme/api", domain: "api.acme.com", version: "v452" },
  { id: "res_apidb", projectId: "proj_api", environmentId: "env_api_prod", serverId: "srv_db_1", name: "api-postgres", kind: "postgres", status: "running", lastDeployAt: "2027-02-28T08:00:00Z", version: "16.2" },
  { id: "res_apis3", projectId: "proj_api", environmentId: "env_api_prod", serverId: "srv_store_1", name: "assets", kind: "s3", status: "running", lastDeployAt: "2027-02-25T12:00:00Z" },
  { id: "res_worker", projectId: "proj_api", environmentId: "env_api_dev", serverId: "srv_gen_2", name: "worker", kind: "app", status: "degraded", lastDeployAt: "2027-03-01T16:45:00Z", repo: "acme/api", version: "v451" },
  { id: "res_llm", projectId: "proj_mllab", environmentId: "env_mllab_prod", serverId: "srv_gpu_1", name: "llama-3-70b", kind: "llm", status: "degraded", lastDeployAt: "2027-03-01T11:00:00Z", version: "vllm 0.6" },
  { id: "res_site", projectId: "proj_site", environmentId: "env_site_prod", serverId: "srv_nw_1", name: "marketing", kind: "app", status: "running", lastDeployAt: "2027-03-02T07:00:00Z", repo: "nw/site", domain: "northwind.com", version: "v33" },
  { id: "res_siteblog", projectId: "proj_site", environmentId: "env_site_prod", serverId: "srv_nw_1", name: "blog-mongo", kind: "mongo", status: "running", lastDeployAt: "2027-02-15T09:00:00Z", version: "7.0" },
];

export const members: Member[] = [
  { id: "u1", name: "Strahinja Polovina", email: "strahinja@sigmajunction.com", role: "Org Admin" },
  { id: "u2", name: "Mila Jovanović", email: "mila@acme.com", role: "Project Admin" },
  { id: "u3", name: "Nikola Petrović", email: "nikola@acme.com", role: "Developer" },
  { id: "u4", name: "Ana Kovač", email: "ana@acme.com", role: "Developer" },
];
