// @vitest-environment jsdom
//
// Removing an environment cascades every resource inside it — the CP's
// DeleteEnvironment runs cascadeResourceCleanupTx with no refusal and no force
// flag, so the running Postgres, Redis and app all go, along with their
// deployment history. The dialog used to be prose only: no count, no list, and
// a single red button, while deleting ONE resource already demands the
// resource's name be typed. The confirmation bar was inverted with respect to
// blast radius, and adjacent environment tabs ("production", "sandbox") made
// picking the wrong one easy (SIGMA-314).
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn(), push: vi.fn(), replace: vi.fn() }),
}));
vi.mock("sonner", () => {
  const toast = Object.assign(vi.fn(), {
    success: vi.fn(),
    error: vi.fn(),
    info: vi.fn(),
    warning: vi.fn(),
    message: vi.fn(),
  });
  return { toast };
});
vi.mock("@/server/actions/projects", () => ({
  attachServerToEnv: vi.fn(),
  deleteEnvironment: vi.fn(),
  detachServerFromEnv: vi.fn(),
  createEnvironment: vi.fn(),
  deleteProject: vi.fn(),
  renameProject: vi.fn(),
  setEnvironmentProduction: vi.fn(),
}));

import type { DeployTarget } from "@/components/dashboard/resources/resource-meta";

// The page is a client component wired to next/navigation, sonner and a fan of
// server actions; none of that is what these tests are about.
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn(), push: vi.fn(), replace: vi.fn() }),
}));
vi.mock("sonner", () => {
  const toast = Object.assign(vi.fn(), {
    success: vi.fn(),
    error: vi.fn(),
    info: vi.fn(),
    warning: vi.fn(),
  });
  return { toast };
});
vi.mock("@/server/actions/projects", () => ({
  attachServerToEnv: vi.fn(),
  deleteEnvironment: vi.fn(),
  detachServerFromEnv: vi.fn(),
  createEnvironment: vi.fn(),
  setEnvironmentProduction: vi.fn(),
}));

// The wizard itself is six steps of its own; what this file asks is what the
// PAGE hands it, so it is replaced by a prop recorder.
const wizardTargets: DeployTarget[][] = [];
vi.mock("@/components/dashboard/resources/deploy-wizard", () => ({
  DeployWizard: (props: { targets: DeployTarget[] }) => {
    wizardTargets.push(props.targets);
    return <div data-testid="wizard" />;
  },
}));

import { deleteEnvironment } from "@/server/actions/projects";
import { DeleteEnvironmentButton } from "./project-detail-view";
import type { EnvPanel } from "@/server/queries";

type EnvResource = EnvPanel["resources"][number];

function res(id: string, name: string, kind: string): EnvResource {
  return {
    id,
    projectId: "prj_1",
    environmentId: "env_1",
    serverId: "srv_1",
    clusterId: null,
    name,
    kind,
    status: "running",
    repo: null,
    branch: null,
    domain: null,
    version: null,
    lastDeployAt: null,
    createdAt: new Date("2026-08-01T10:00:00Z"),
    latestDeploy: null,
  } as unknown as EnvResource;
}

const RESOURCES: EnvResource[] = [
  res("res_1", "api", "app"),
  res("res_2", "db", "postgres"),
  res("res_3", "cache", "redis"),
];

function renderButton(resources: EnvResource[] = RESOURCES) {
  render(
    <DeleteEnvironmentButton environmentId="env_1" name="production" resources={resources} />
  );
  fireEvent.click(screen.getByRole("button", { name: /Remove/ }));
}

function confirmButton(): HTMLButtonElement {
  return screen.getByRole("button", { name: /^Remove environment$/ }) as HTMLButtonElement;
}

afterEach(() => {
  cleanup();
  wizardTargets.length = 0;
  vi.clearAllMocks();
});

describe("DeleteEnvironmentButton", () => {
  it("stays disabled until the environment's name is typed", async () => {
    renderButton();
    await screen.findByRole("dialog");

    expect(confirmButton().disabled).toBe(true);

    const input = screen.getByLabelText(/type/i);
    fireEvent.change(input, { target: { value: "produc" } });
    expect(confirmButton().disabled).toBe(true);

    fireEvent.change(input, { target: { value: "production" } });
    expect(confirmButton().disabled).toBe(false);

    fireEvent.click(confirmButton());
    expect(deleteEnvironment).toHaveBeenCalledWith({ environmentId: "env_1" });
  });

  it("renders one row per resource it will destroy, with a count", async () => {
    renderButton();
    const dialog = await screen.findByRole("dialog");

    const rows = within(dialog).getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringContaining("api"),
      expect.stringContaining("db"),
      expect.stringContaining("cache"),
    ]);
    // The kinds matter as much as the names: "db" alone doesn't tell you a
    // Postgres is about to be cascaded away.
    expect(rows[1]?.textContent).toContain("postgres");
    expect(dialog.textContent).toMatch(/3 resources/);
  });

  it("an empty environment says so instead of listing nothing", async () => {
    renderButton([]);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryAllByRole("listitem")).toHaveLength(0);
    expect(dialog.textContent).toMatch(/no resources/i);
  });
});

import { ProjectDetailView } from "./project-detail-view";

type Props = React.ComponentProps<typeof ProjectDetailView>;
type Panel = Props["panels"][number];

const GPU = {
  vendor: "nvidia",
  model: "NVIDIA L4",
  count: 1,
  vramBytesPerGpu: 24 * 1024 ** 3,
  vramBytesTotal: 24 * 1024 ** 3,
};

function makePanel(): Panel {
  return {
    env: {
      id: "env_1",
      projectId: "proj_1",
      name: "production",
      isProduction: true,
      createdAt: new Date("2026-01-01T00:00:00Z"),
    },
    servers: [
      {
        id: "srv_1",
        orgId: "org_1",
        name: "gpu-1",
        type: "gpu",
        source: "byo",
        provider: "hetzner",
        region: "fsn1",
        status: "connected",
        agentVersion: "1.0.0",
        ip: "1.2.3.4",
        meshIp: "10.8.0.2",
        cpu: 8,
        memGb: 32,
        byoVpn: false,
        connectedAt: new Date("2026-01-01T00:00:00Z"),
        facts: { hostname: "gpu-1", gpu: GPU },
        incompatibleReasons: [],
        nameAuto: false,
      },
    ],
    resources: [],
  } as unknown as Panel;
}

function renderPage() {
  return render(
    <ProjectDetailView
      project={{ id: "proj_1", name: "Acme", slug: "acme", description: "" }}
      panels={[makePanel()]}
      orgServers={[
        { id: "srv_1", name: "gpu-1", type: "gpu", region: "fsn1", status: "connected" },
      ]}
      orgId="org_1"
      cpMode
    />
  );
}

describe("ProjectDetailView deploy wizard targets", () => {
  it("wizard targets from a project page carry gpu facts", () => {
    renderPage();
    fireEvent.click(screen.getAllByRole("button", { name: /Add resource/ })[0]);

    const targets = wizardTargets.at(-1);
    expect(targets).toBeDefined();
    const server = targets![0].environments[0].servers[0];
    // Without this the SIGMA-214 VRAM fit check reads every target as UNKNOWN
    // and warns about nothing, so the same 70B model the Resources-page wizard
    // flags is offered here and refused by the control plane at create time.
    expect(server.gpu).toEqual(GPU);
  });
});
