/**
 * Marketing copy deck — the single source of truth for every public-site section.
 *
 * Every figure and claim here is drawn from the canonical SigmaHub knowledge base
 * (YouTrack SIGMA-A-1..A-3). If a number changes in the KB, change it here.
 *
 *  - Pricing: EUR 5 / unit / month (server weighted by management cost:
 *    ordinary 1, Kubernetes node 2, GPU 4), all features included, no add-ons.
 *    Free up to 3 connected servers. No Enterprise tier on the public site.
 *  - SigmaHub never sells, rents or resells servers and never marks up infrastructure.
 */

import type { LucideIcon } from "lucide-react";
import {
  Boxes,
  Cpu,
  Database,
  GaugeCircle,
  GitBranch,
  HardDrive,
  LifeBuoy,
  Save,
  Server,
  ShieldCheck,
} from "lucide-react";

/* -------------------------------------------------------------------------- */
/*  Brand / hero                                                              */
/* -------------------------------------------------------------------------- */

export const HERO = {
  eyebrow: "Managed PaaS for your own servers",
  title: "Run apps, databases, AI and Kubernetes on servers you own",
  subtitle:
    "SigmaHub is a managed cloud control plane for your own machines — Git deploys, managed databases, S3 storage, GPU/LLM inference, monitoring, disaster recovery and Kubernetes, from one dashboard. You pay for the platform, never for the servers.",
  primaryCta: { label: "Start free", href: "/dashboard" },
  secondaryCta: { label: "Talk to us", href: "#pricing" },
  note: "Free for up to 3 units · €5 per unit after that · every feature included",
} as const;

/* -------------------------------------------------------------------------- */
/*  "Works with your stack" strip                                            */
/* -------------------------------------------------------------------------- */

export const WORKS_WITH = {
  heading: "Runs on the servers and tools you already use",
  groups: [
    {
      label: "Your servers",
      items: ["Hetzner", "OVH", "Latitude.sh", "Bare metal", "On-prem"],
    },
    {
      label: "Integrates with",
      items: ["GitHub", "GitLab", "Docker", "Cloudflare", "Slack", "Telegram"],
    },
  ],
} as const;

/* -------------------------------------------------------------------------- */
/*  Problem / why-now                                                        */
/* -------------------------------------------------------------------------- */

export const PROBLEM = {
  eyebrow: "Why SigmaHub",
  title: "The cloud got expensive. Running your own servers got complicated.",
  subtitle:
    "Cloud repatriation went mainstream — 21% of workloads are already back on private infrastructure (Flexera, 2025). But leaving the cloud usually means hand-assembling a dozen tools nobody wants to own.",
  cards: [
    {
      icon: GaugeCircle,
      title: "Cloud cost explosion",
      body: "Bare-metal and EU hosts run 2.5–3.5× cheaper than hyperscalers for steady-state compute. 37signals cut ~$2M/yr leaving AWS — then replaced a $1.5M/yr S3 bill with hardware costing under $200k/yr to run.",
    },
    {
      icon: Boxes,
      title: "The ops complexity tax",
      body: "Self-hosting today means Terraform, Docker, a reverse proxy, TLS, Prometheus, Loki, backups, WireGuard, MinIO and k3s — each with its own failure mode. A dedicated SRE costs €90–120k/yr.",
    },
    {
      icon: Cpu,
      title: "AI inference sticker shock",
      body: "Managed inference is priced per token or per GPU-hour — an H100 endpoint runs ~$4,670/mo. The same open model on a rented GPU costs a fraction, if someone wires up vLLM, drivers, keys and metering.",
    },
  ],
} as const;

/* -------------------------------------------------------------------------- */
/*  Product pillars                                                          */
/* -------------------------------------------------------------------------- */

export type Pillar = {
  icon: LucideIcon;
  title: string;
  description: string;
};

export const PILLARS_SECTION = {
  eyebrow: "One platform, every primitive",
  title: "Everything a modern cloud gives you — on hardware you control",
  subtitle:
    "Eight production primitives, one control plane, one price. Disaster recovery, GPU/LLM serving and Kubernetes are included — never add-ons.",
} as const;

export const PILLARS: Pillar[] = [
  {
    icon: Server,
    title: "Server onboarding",
    description:
      "Bring any Linux server over SSH — Hetzner, OVH, bare metal or on-prem. One-line agent install, deploy-ready in minutes, even behind NAT.",
  },
  {
    icon: GitBranch,
    title: "App deploys from Git",
    description:
      "Push to GitHub or GitLab and ship. Dockerfile & compose detection, zero-downtime rollouts, automatic Let's Encrypt TLS and preview environments.",
  },
  {
    icon: Database,
    title: "Managed databases",
    description:
      "PostgreSQL, MySQL, Redis and MongoDB with verified, restorable backups to any S3-compatible target. Point-in-time recovery on the roadmap.",
  },
  {
    icon: HardDrive,
    title: "S3 object storage",
    description:
      "Turn a storage box into S3-compatible buckets — MinIO or SeaweedFS — with scoped access keys, quotas, versioning and usage metering.",
  },
  {
    icon: Cpu,
    title: "GPU / LLM inference",
    description:
      "Serve open models on your own GPUs. OpenAI-compatible endpoints, managed API keys, per-key token metering and DCGM GPU telemetry — drivers bootstrapped for you.",
  },
  {
    icon: GaugeCircle,
    title: "Monitoring & alerting",
    description:
      "Metrics, logs and alerts for every server and resource, zero-config. Sane default alerts on creation — alerting is opt-out, not opt-in.",
  },
  {
    icon: LifeBuoy,
    title: "Disaster recovery",
    description:
      "Per-environment recovery plans with continuously replicated backups, health-checked automatic failover and DNS flip. Target RTO < 15 min, RPO ≤ 5 min.",
  },
  {
    icon: Boxes,
    title: "Kubernetes clusters",
    description:
      "Group owned servers into a managed HA k3s cluster over the mesh, with one-click upgrades and revocable kubeconfig export. No distro maintenance.",
  },
];

/* -------------------------------------------------------------------------- */
/*  Architecture / security                                                  */
/* -------------------------------------------------------------------------- */

export const ARCHITECTURE = {
  eyebrow: "Architecture",
  title: "One control plane over your infrastructure",
  subtitle:
    "SigmaHub orchestrates typed servers over an encrypted WireGuard mesh. You keep the hardware; we keep it managed — and we never resell it.",
  controlPlane: {
    title: "SigmaHub control plane",
    note: "Orchestration · scheduling · metering · observability",
  },
  mesh: "WireGuard encrypted mesh · outbound-only agents",
  servers: [
    { label: "General", icon: Server, note: "apps · k8s" },
    { label: "Storage", icon: HardDrive, note: "S3 · backups" },
    { label: "Database", icon: Database, note: "postgres · redis" },
    { label: "GPU", icon: Cpu, note: "LLM · inference" },
  ],
  security: [
    {
      icon: ShieldCheck,
      title: "Secure by default",
      body: "WireGuard mesh between control plane and every server, outbound-only signed agents, and envelope-encrypted secrets (AES-256-GCM).",
    },
    {
      icon: Server,
      title: "Works behind NAT",
      body: "Agents dial out only — no inbound ports beyond your app's 80/443. Onboard servers with no public SSH over an optional VPN or jump host.",
    },
    {
      icon: Save,
      title: "Full audit trail",
      body: "Every key, deploy and failover is logged. Signed, cosign-verified agent releases and a SOC 2 / GDPR compliance roadmap.",
    },
  ],
} as const;

/* -------------------------------------------------------------------------- */
/*  Comparison table                                                         */
/* -------------------------------------------------------------------------- */

export const COMPARE = {
  eyebrow: "How we compare",
  title: "Coolify's economics. A GPU cloud's inference. Railway's UX. Unified.",
  subtitle:
    "The deploy-PaaS tools solved one slice. SigmaHub is the whole platform — on servers you own, secure by default.",
  columns: [
    "SigmaHub",
    "Coolify / Dokploy",
    "Portainer / Rancher",
    "Railway / Render / Fly",
    "GPU clouds",
  ],
  rows: [
    {
      capability: "App deploys on your own servers",
      values: ["yes", "yes", "partial", "no", "no"],
    },
    {
      capability: "First-class S3 storage on your servers",
      values: ["yes", "no", "no", "no", "no"],
    },
    {
      capability: "GPU/LLM serving with keys + token metering",
      values: ["yes", "minimal", "no", "no", "on their GPUs"],
    },
    {
      capability: "Automated DR with failover + DNS flip",
      values: ["yes", "no", "no", "implicit", "no"],
    },
    {
      capability: "Managed Kubernetes on owned servers",
      values: ["yes", "no", "ops-heavy", "no", "no"],
    },
    {
      capability: "Security by default (mesh, signed agent)",
      values: ["yes", "basic", "varies", "managed", "managed"],
    },
    {
      capability: "One all-inclusive price, never the servers",
      values: ["yes", "hobby tier", "per-node", "resold compute", "GPU margin"],
    },
  ],
} as const;

/* -------------------------------------------------------------------------- */
/*  Cost math                                                                */
/* -------------------------------------------------------------------------- */

export const COST = {
  eyebrow: "The economics",
  title: "Own-server cost profile, cloud-grade experience",
  subtitle:
    "Keep the cost of hardware you control — pay a flat platform fee instead of metered public-cloud markups. Indicative, steady-state figures.",
  rows: [
    {
      line: "Single-GPU inference box, 24/7",
      managed: "$730/mo",
      managedNote: "AWS g5.xlarge (A10G)",
      owned: "€184/mo",
      ownedNote: "Hetzner RTX 4000 Ada",
    },
    {
      line: "H100 dedicated endpoint, 24/7",
      managed: "$4,670/mo",
      managedNote: "Together AI @ $6.49/hr",
      owned: "$1,500–2,400/mo",
      ownedNote: "rented H100 bare metal",
    },
    {
      line: "18 PB object storage",
      managed: "$1.5M/yr",
      managedNote: "AWS S3 (37signals actuals)",
      owned: "< $200k/yr",
      ownedNote: "opex after hardware buy",
    },
  ],
  kicker:
    "A 10-server fleet runs €35/month on SigmaHub — three units free, storage, DR and Kubernetes included — versus per-service, per-GB and per-token billing on public cloud.",
} as const;

/* -------------------------------------------------------------------------- */
/*  Use cases / personas                                                     */
/* -------------------------------------------------------------------------- */

export const USE_CASES = {
  eyebrow: "Who it's for",
  title: "Built for the teams the cloud priced out",
  subtitle:
    "From a solo agency running client apps to an AI team serving its own models — one mental model covers a single VPS and a 50-server fleet alike.",
  cases: [
    {
      icon: GitBranch,
      audience: "Agencies & indie devs",
      pain: "Backup discipline per client, TLS toil, no per-client isolation.",
      gain: "One project per client, isolated environments, backups you can actually restore — starting free on up to 3 servers.",
    },
    {
      icon: GaugeCircle,
      audience: "Startup CTOs",
      pain: "PaaS bills that scale with traffic, vendor lock-in, no EU data story.",
      gain: "Railway-grade ergonomics on bare metal — typically 60–80% cheaper compute, with a platform fee that's a rounding error.",
    },
    {
      icon: Cpu,
      audience: "AI teams",
      pain: "CUDA/driver toil, no token metering, data leaving the perimeter.",
      gain: "Self-hosted OpenAI-compatible inference on GPUs you control, with metering and keys — the wedge no rival serves.",
    },
    {
      icon: ShieldCheck,
      audience: "SME platform teams",
      pain: "ISO 27001 / GDPR audits, SSO, DR that's a binder not a system.",
      gain: "Audit logging, RBAC, encrypted secrets and rehearsed DR on infrastructure you fully control — SSO and fleet-scale orchestration as you grow.",
    },
  ],
} as const;

/* -------------------------------------------------------------------------- */
/*  Onboarding / CLI                                                         */
/* -------------------------------------------------------------------------- */

export const ONBOARDING = {
  eyebrow: "Onboarding",
  title: "From bare server to production in minutes",
  subtitle:
    "Install the CLI, connect a server, and deploy. SigmaHub handles the mesh, the runtime and the rollout — no SSH config, no Docker installs.",
  cta: { label: "Read the docs", href: "/dashboard" },
  terminalTitle: "sigma — zsh",
  lines: [
    { prompt: "$", command: "curl -fsSL https://get.sigmahub.io | sh" },
    { prompt: "$", command: "sigma login" },
    { prompt: "$", command: "sigma servers connect edge-fra-1" },
    { prompt: "$", command: "sigma deploy ./webshop --env production" },
  ],
  success: "✓ webshop deployed · https://webshop.acme.sigma.app",
} as const;

/* -------------------------------------------------------------------------- */
/*  Pricing                                                                  */
/* -------------------------------------------------------------------------- */

export const PRICING = {
  eyebrow: "Pricing",
  title: "One number. The size of your fleet, weighted by what it takes to run.",
  subtitle:
    "€5 per unit per month. An ordinary server is one unit, a Kubernetes node two, a GPU server four — because that is what each costs us to manage, never what your hardware costs. Every feature is included. No add-ons, no egress fees, no per-token GPU billing, no per-seat tax.",
  tiers: [
    {
      name: "Free",
      price: "€0",
      unit: "forever",
      tagline: "For side projects and your first deploys.",
      cta: { label: "Start free", href: "/dashboard" },
      featured: false,
      features: [
        "Up to 3 units — three ordinary servers",
        "Every feature included — nothing gated",
        "Git deploys, databases & S3 storage",
        "GPU/LLM serving & Kubernetes",
        "Monitoring with default alerts",
        "Community support",
      ],
    },
    {
      name: "Cloud",
      price: "€5",
      unit: "/ unit / month",
      tagline: "Ordinary server 1 · Kubernetes node 2 · GPU server 4.",
      cta: { label: "Start free", href: "/dashboard" },
      featured: true,
      features: [
        "Unlimited servers, clusters and GPUs",
        "Disaster recovery & automatic failover",
        "GPU/LLM serving with token metering",
        "Managed Kubernetes (k3s HA)",
        "Verified backups & audit log",
        "No add-ons, no egress, no surprises",
      ],
    },
  ],
  footnote:
    "Your first three units are always free, so three ordinary servers still cost nothing. Beyond that it is €5 per unit — SigmaHub never sells, rents or marks up your infrastructure, and a heavier weight only ever reflects more management on our side. That's the whole deal.",
} as const;

/* -------------------------------------------------------------------------- */
/*  FAQ                                                                      */
/* -------------------------------------------------------------------------- */

export const FAQ = {
  eyebrow: "FAQ",
  title: "Questions, answered",
  items: [
    {
      q: "Do you sell or host servers?",
      a: "Never. You bring your own servers — any provider, bare metal or on-prem — or connect your own provider account, and the provider bills you directly. SigmaHub earns €0 on infrastructure and charges only for the management layer.",
    },
    {
      q: "How does pricing work, exactly?",
      a: "€5 × your number of units per month, with the first three free. An ordinary server (including databases and storage) is 1 unit, a Kubernetes node 2, a GPU server 4. Every feature is included — disaster recovery, GPU/LLM serving, Kubernetes, S3 and backups — with no add-ons, no egress fees and no per-token GPU billing.",
    },
    {
      q: "Why does a GPU server count as four units?",
      a: "Because managing it is four times the work, not because the machine is expensive — it is your machine and we earn nothing on it. A GPU host means drivers, CUDA versions, the model runtime and token metering; a Kubernetes node means cluster lifecycle, upgrades and networking. An ordinary Docker host needs none of that, which is why it stays at one unit and the price you already know.",
    },
    {
      q: "What about servers behind NAT or a firewall?",
      a: "The sigmad agent is outbound-only over a WireGuard mesh, so it works behind NAT/CGNAT with no inbound ports beyond your app's 80/443. Servers without public SSH can be onboarded over an optional VPN or jump host.",
    },
    {
      q: "Is my data secure?",
      a: "Security is default-on: a WireGuard mesh between the control plane and every server, outbound-only cosign-verified agents, envelope-encrypted secrets (AES-256-GCM) and a full audit log — with a SOC 2 / GDPR compliance roadmap.",
    },
    {
      q: "Which providers and stacks are supported?",
      a: "Any Linux server (Ubuntu/Debian at beta) — onboarding is a one-line SSH bootstrap, wherever the box runs (Hetzner, OVH, Latitude.sh, bare metal). Deploy from GitHub or GitLab, build from Dockerfile or compose, and flip DNS via Cloudflare.",
    },
    {
      q: "Do I still need a DevOps or SRE hire?",
      a: "That's the point — no. SigmaHub replaces the hand-assembled stack of Terraform, Docker, a proxy, TLS, Prometheus, Loki, backups, WireGuard, MinIO and k3s with one managed control plane.",
    },
  ],
} as const;

/* -------------------------------------------------------------------------- */
/*  Closing CTA                                                              */
/* -------------------------------------------------------------------------- */

export const CLOSING = {
  title: "Bring your servers. We'll bring the cloud.",
  subtitle: "Connect your first three servers free and deploy in minutes.",
  primaryCta: { label: "Start free", href: "/dashboard" },
  secondaryCta: { label: "Log in", href: "/login" },
} as const;

/* -------------------------------------------------------------------------- */
/*  Navigation & footer                                                      */
/* -------------------------------------------------------------------------- */

export const NAV_LINKS = [
  { label: "Product", href: "#product" },
  { label: "Architecture", href: "#architecture" },
  { label: "Compare", href: "#compare" },
  { label: "Pricing", href: "#pricing" },
  { label: "Docs", href: "#docs" },
] as const;

export const FOOTER_COLUMNS = [
  {
    heading: "Product",
    links: [
      { label: "Overview", href: "#product" },
      { label: "Architecture", href: "#architecture" },
      { label: "Compare", href: "#compare" },
      { label: "Pricing", href: "#pricing" },
      { label: "Docs", href: "#docs" },
    ],
  },
  {
    heading: "Company",
    links: [
      { label: "About", href: "#" },
      { label: "Blog", href: "#" },
      { label: "Careers", href: "#" },
      { label: "Contact", href: "#" },
    ],
  },
  {
    heading: "Legal",
    links: [
      { label: "Privacy", href: "#" },
      { label: "Terms", href: "#" },
      { label: "Security", href: "#" },
      { label: "DPA", href: "#" },
    ],
  },
] as const;
