// @vitest-environment jsdom
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";

// The page is a client component wired to next/navigation, sonner and a fan of
// server actions. None of those are what these tests are about: every test here
// asks what a control ON THE PAGE actually does, so the boundary is stubbed and
// the component is rendered for real.
const refresh = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh, push: vi.fn(), replace: vi.fn() }),
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
vi.mock("@/server/actions/resources", () => ({
  advanceDeployment: vi.fn(),
  deleteResource: vi.fn(),
  deployResource: vi.fn(),
  requestVolumeDeleteConfirm: vi.fn(),
  confirmVolumeDelete: vi.fn(),
}));
vi.mock("@/server/actions/domains", () => ({
  attachDomain: vi.fn(),
  detachDomain: vi.fn(),
}));
vi.mock("@/server/actions/deployments", () => ({
  rollbackDeployment: vi.fn(),
  fetchDeployLogs: vi.fn(),
}));
vi.mock("@/server/actions/databases", () => ({ revealDatabaseConnection: vi.fn() }));
vi.mock("@/server/actions/s3", () => ({
  revealS3Connection: vi.fn(),
  listBuckets: vi.fn(),
  createBucket: vi.fn(),
  deleteBucket: vi.fn(),
}));
vi.mock("@/server/actions/compose", () => ({ setComposePlacements: vi.fn() }));
vi.mock("@/server/actions/dns", () => ({ getDomainDNS: vi.fn() }));
vi.mock("@/server/actions/backups", () => ({
  setBackupPolicy: vi.fn(),
  runBackupNow: vi.fn(),
  restoreBackup: vi.fn(),
  verifyBackup: vi.fn(),
}));
vi.mock("@/server/actions/secrets", () => ({
  createSecret: vi.fn(),
  deleteSecret: vi.fn(),
  revealSecret: vi.fn(),
  updateSecret: vi.fn(),
}));

import { ResourceDetail } from "./resource-detail";

type Detail = React.ComponentProps<typeof ResourceDetail>["detail"];

function makeDetail(over: Partial<Detail["resource"]> = {}, rest: Partial<Detail> = {}): Detail {
  return {
    resource: {
      id: "res_1",
      name: "api",
      kind: "app",
      status: "running",
      repo: "acme/api",
      domain: "app.example.com",
      version: "v1.2.3",
      lastDeployAt: new Date("2026-08-01T10:00:00Z"),
      serverId: "srv_1",
      ...over,
    },
    projectName: "Acme",
    envName: "production",
    server: { id: "srv_1", name: "edge-1", type: "app" },
    cluster: null,
    deployments: [],
    secrets: [],
    canManage: true,
    ...rest,
  };
}

/** CP mode = a non-null cpTelemetry payload; everything else defaults. */
function renderCp(props: Partial<React.ComponentProps<typeof ResourceDetail>> = {}) {
  return render(
    <ResourceDetail
      detail={makeDetail()}
      orgId="org_1"
      cpTelemetry={{ pipeline: true, metrics: [], logs: [] }}
      {...props}
    />
  );
}

function openTab(name: string) {
  fireEvent.click(screen.getByRole("tab", { name }));
}

function dangerZone(): HTMLElement {
  const card = screen.getByText("Danger zone").closest('[data-slot="card"]');
  if (!card) throw new Error("danger zone card not found");
  return card as HTMLElement;
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ResourceDetail danger zone", () => {
  it("the danger zone exposes no control that only fires a toast", () => {
    renderCp();
    openTab("Settings");

    // Every control in a card headed "These actions are irreversible" must
    // either open a confirmation dialog that runs a server action, or not be
    // here at all. A button whose whole handler is a toast reports an
    // irreversible action it never performed (SIGMA-234, and SIGMA-162 one card
    // away).
    const buttons = within(dangerZone()).queryAllByRole("button");
    const labels = buttons.map((b) => (b.textContent ?? "").trim());
    expect(labels.sort()).toEqual(["Delete", "Delete a data volume"].sort());
    expect(labels).not.toContain("Stop");
  });
});

describe("ResourceDetail telemetry states", () => {
  it("a telemetry fetch failure appears in the load-failure banner and not as the empty state", () => {
    renderCp({
      loadFailures: ["logs and metrics"],
      cpTelemetry: { pipeline: true, unreadable: true, metrics: [], logs: [] },
    });
    openTab("Logs");

    expect(screen.getByText(/Some of this page couldn't be loaded/i)).toBeTruthy();
    expect(screen.getByText(/didn't answer for logs and metrics/i)).toBeTruthy();
    // "No telemetry received yet" asserts the pipeline is configured and the
    // container produced nothing — the opposite of the truth when the read
    // failed (SIGMA-236).
    expect(screen.queryByText(/No telemetry received yet/i)).toBeNull();
  });

  it("a reachable but empty pipeline still says no telemetry has arrived", () => {
    renderCp({ cpTelemetry: { pipeline: true, metrics: [], logs: [] } });
    openTab("Logs");
    expect(screen.getByText(/No telemetry received yet/i)).toBeTruthy();
  });

  it("an unconfigured pipeline says so", () => {
    renderCp({ cpTelemetry: { pipeline: false, metrics: [], logs: [] } });
    openTab("Logs");
    expect(screen.getByText(/Telemetry pipeline not configured/i)).toBeTruthy();
  });
});
