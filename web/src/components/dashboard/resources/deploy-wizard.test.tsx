// @vitest-environment jsdom
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { WIZARD_RESUME_KEY, encodeWizardDraft } from "@/lib/wizard/resume";

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

const createResource = vi.fn();
vi.mock("@/server/actions/resources", () => ({ createResource: (...a: unknown[]) => createResource(...a) }));
vi.mock("@/server/actions/secrets", () => ({ createSecretAction: vi.fn() }));
vi.mock("@/server/actions/git", () => ({
  detectRepo: vi.fn(),
  getGitAppInfo: vi.fn().mockResolvedValue({ slug: "" }),
  wireRepoToEnvironment: vi.fn(),
}));
vi.mock("@/server/actions/capabilities", () => ({
  getEngineCapabilities: vi.fn().mockResolvedValue(null),
}));
vi.mock("@/server/actions/databases", () => ({ revealDatabaseConnection: vi.fn() }));
vi.mock("@/server/actions/s3", () => ({ revealS3Connection: vi.fn() }));

import { DeployWizard } from "./deploy-wizard";

const TARGETS = [
  {
    id: "proj_1",
    name: "Acme",
    environments: [
      {
        id: "env_1",
        name: "production",
        servers: [
          {
            id: "srv_1",
            name: "db-1",
            type: "database",
            provider: "hetzner",
            region: "fsn1",
            status: "connected",
          },
        ],
      },
    ],
  },
];

/** Opens the wizard already holding a resumable draft, so the flow starts on
 *  step 1 with the kind, the project, the environment and the server chosen —
 *  the state a user reaches by hand — and reaching Deploy is three clicks. */
function openWizard() {
  window.sessionStorage.setItem(
    WIZARD_RESUME_KEY,
    encodeWizardDraft({
      kind: "postgres",
      name: "pg-prod",
      projectId: "proj_1",
      environmentId: "env_1",
      serverId: "srv_1",
    })
  );
  return render(
    <DeployWizard open resume onOpenChange={vi.fn()} targets={TARGETS} orgId="org_1" cpMode />
  );
}

function click(name: RegExp | string) {
  fireEvent.click(screen.getByRole("button", { name }));
}

/** Walk step 1 → engine → target → Deploy. */
async function deploy() {
  click("Continue");
  click("Continue");
  click("Deploy");
  await screen.findByText("Create failed");
}

beforeEach(() => {
  createResource.mockReset();
  createResource.mockRejectedValue(new Error("A resource named pg-prod already exists."));
});

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
  vi.clearAllMocks();
});

describe("DeployWizard failed create", () => {
  it('a failed create offers retry and does not say "You\'re all set."', async () => {
    openWizard();
    await deploy();

    expect(createResource).toHaveBeenCalledTimes(1);
    // The footer used to key on `done = createState === "done" || "error"`, so
    // a red "Create failed" panel sat under the words "You're all set."
    expect(screen.queryByText("You're all set.")).toBeNull();

    const retry = screen.getByRole("button", { name: /Retry/ }) as HTMLButtonElement;
    expect(retry.disabled).toBe(false);

    // createStartedRef latched on the first run of the create effect and was
    // cleared only by a re-open, so the create could never be run again.
    createResource.mockResolvedValue({ id: "res_1" });
    fireEvent.click(retry);
    await waitFor(() => expect(createResource).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("Resource created")).toBeTruthy();
    expect(screen.getByText("You're all set.")).toBeTruthy();
  });

  it("a failed create can go back to the previous step with the answers intact", async () => {
    openWizard();
    await deploy();

    click(/Back/);
    // The target step, still holding the server that was picked.
    expect(screen.getByLabelText("Environment")).toBeTruthy();
    expect(screen.queryByText("Create failed")).toBeNull();

    // Forward again re-runs the create rather than showing the old failure.
    createResource.mockResolvedValue({ id: "res_1" });
    click("Deploy");
    await waitFor(() => expect(createResource).toHaveBeenCalledTimes(2));
  });
});
