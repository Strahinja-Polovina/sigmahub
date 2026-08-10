// @vitest-environment jsdom
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

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

afterEach(() => {
  cleanup();
  wizardTargets.length = 0;
  vi.clearAllMocks();
});

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
