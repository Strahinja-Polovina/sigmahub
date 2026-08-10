// @vitest-environment jsdom
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";

// The page is a client component wired to next/navigation, sonner and a fan of
// server actions. None of those are what these tests are about: every test here
// asks what a control ON THE PAGE actually does, so the boundary is stubbed and
// the component is rendered for real.
const refresh = vi.fn();
let searchParams = new URLSearchParams();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh, push: vi.fn(), replace: vi.fn() }),
  useSearchParams: () => searchParams,
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

/** The value cell of a FactRow, found by its label. */
function factRow(label: string): HTMLElement {
  const cell = screen.getByText(label).parentElement;
  if (!cell) throw new Error(`no fact row for ${label}`);
  return cell;
}

function dangerZone(): HTMLElement {
  const card = screen.getByText("Danger zone").closest('[data-slot="card"]');
  if (!card) throw new Error("danger zone card not found");
  return card as HTMLElement;
}

afterEach(() => {
  cleanup();
  searchParams = new URLSearchParams();
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

describe("ResourceDetail links to the running app", () => {
  it("the Open control is an anchor to the resource's domain", () => {
    renderCp();
    const open = screen.getByRole("link", { name: /^Open$/ });
    expect(open.getAttribute("href")).toBe("https://app.example.com");
    expect(open.getAttribute("target")).toBe("_blank");
    expect(open.getAttribute("rel")).toContain("noopener");
  });

  it("the domain chip navigates instead of cancelling its own click", () => {
    renderCp();
    const chip = screen.getByRole("link", { name: /app\.example\.com/ });
    expect(chip.getAttribute("href")).toBe("https://app.example.com");
    expect(chip.getAttribute("target")).toBe("_blank");
    // It used to be an <a> with onClick={(e) => e.preventDefault()} — an anchor
    // that looks broken rather than decorative (SIGMA-238).
    const click = new MouseEvent("click", { bubbles: true, cancelable: true });
    chip.dispatchEvent(click);
    expect(click.defaultPrevented).toBe(false);
  });

  it("a resource with no domain offers no Open control at all", () => {
    renderCp({ detail: makeDetail({ domain: null }) });
    expect(screen.queryByRole("link", { name: /^Open$/ })).toBeNull();
  });
});

describe("ResourceDetail settings facts", () => {
  it("the Settings tab reflects a manual branch policy", () => {
    renderCp({ autoDeploy: { branch: "main", policy: "manual" } });
    openTab("Settings");

    const row = factRow("Auto-deploy on push");
    // The badge used to be the literal string "Enabled", with no data behind
    // it: a user who mapped their branch as "Manual promote" read this, pushed,
    // and waited for a rollout that never came (SIGMA-240).
    expect(row.textContent).toMatch(/Manual promote/);
    expect(row.textContent).not.toMatch(/Enabled/);
  });

  it("an auto-deploying branch names the branch it deploys from", () => {
    renderCp({ autoDeploy: { branch: "release", policy: "auto" } });
    openTab("Settings");
    expect(factRow("Auto-deploy on push").textContent).toMatch(/On push to.*release/);
  });

  it("a resource with no repository says so rather than claiming auto-deploy", () => {
    renderCp({ detail: makeDetail({ kind: "postgres", repo: null, domain: null }) });
    openTab("Settings");
    expect(factRow("Auto-deploy on push").textContent).toMatch(/Not connected to a repository/);
  });

  it("the health-check row describes the detected probe, or says there is none", () => {
    const { unmount } = renderCp({
      healthCheck: { type: "tcp", port: 3000, intervalSec: 10, source: "default" },
    });
    openTab("Settings");
    expect(factRow("Health checks").textContent).toMatch(/TCP probe on :3000/);
    unmount();

    renderCp();
    openTab("Settings");
    expect(factRow("Health checks").textContent).toMatch(/None/);
    expect(factRow("Health checks").textContent).not.toMatch(/Enabled/);
  });
});

describe("ResourceDetail logs", () => {
  it("Refresh re-fetches the log tail", () => {
    renderCp();
    openTab("Logs");
    fireEvent.click(screen.getByRole("button", { name: /Refresh/ }));
    // The tail is a server render, so refreshing it is refreshing the route.
    expect(refresh).toHaveBeenCalled();
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

describe("ResourceDetail host health", () => {
  /** A resource whose host has gone quiet. There is no statusError — nothing
   *  FAILED, the agent simply stopped answering — and an `llm` has no
   *  Deployments tab, so before SIGMA-251 every surface on this page was empty
   *  and none of them said why. */
  const stuckOnDeadHost = () =>
    makeDetail(
      { kind: "llm", status: "provisioning", repo: null, domain: null, version: null },
      {
        server: {
          id: "srv_1",
          name: "web-01",
          type: "gpu",
          status: "unreachable",
          lastSeenAt: "2026-08-01T08:00:00Z",
        },
      }
    );

  it("a provisioning resource on an unreachable server shows the host's status", () => {
    renderCp({ detail: stuckOnDeadHost() });

    // The banner names the host, says it stopped checking in, and says what
    // that means for this resource — that nothing here converges until the
    // agent comes back.
    const banner = screen.getByText(/has not checked in/i);
    expect(banner.textContent).toMatch(/web-01/);
    expect(banner.textContent).toMatch(/converge/i);

    // And it is a way OUT of this page: the fix is on the server, so the
    // banner links to it rather than leaving the user to find it.
    const link = screen.getByRole("link", { name: /web-01/ });
    expect(link.getAttribute("href")).toBe("/dashboard/servers/srv_1");
  });

  it("a healthy host produces no banner", () => {
    renderCp({
      detail: makeDetail({}, { server: { id: "srv_1", name: "edge-1", type: "general", status: "running" } }),
    });
    expect(screen.queryByText(/has not checked in/i)).toBeNull();
  });

  it("a decommissioning host is called out too", () => {
    renderCp({
      detail: makeDetail(
        { status: "provisioning" },
        { server: { id: "srv_1", name: "edge-1", type: "general", status: "decommissioning" } }
      ),
    });
    expect(screen.getByText(/being disconnected/i)).toBeTruthy();
  });
});

// Overview's per-resource menu links to /dashboard/resources/<id>?tab=logs
// (SIGMA-310). The page rendered <Tabs defaultValue="overview"> and nothing
// read searchParams, so an operator triaging a red resource from the overview
// landed on Overview, assumed they mis-clicked, went back and clicked again.
// The settings page has honoured ?tab since the org switcher started
// deep-linking into it; the two surfaces disagreed about whether it means
// anything.
describe("ResourceDetail honors ?tab", () => {
  function activeTab(): string {
    const tab = screen
      .getAllByRole("tab")
      .find(
        (t) =>
          t.getAttribute("data-selected") !== null || t.getAttribute("aria-selected") === "true"
      );
    return tab?.textContent?.trim() ?? "";
  }

  it("?tab=logs opens the Logs tab", () => {
    searchParams = new URLSearchParams("tab=logs");
    renderCp();
    expect(activeTab()).toBe("Logs");
  });

  it("falls back to Overview for a tab id that does not exist", () => {
    searchParams = new URLSearchParams("tab=nonsense");
    renderCp();
    expect(activeTab()).toBe("Overview");
  });

  it("opens Overview when nothing was asked for", () => {
    renderCp();
    expect(activeTab()).toBe("Overview");
  });
});
