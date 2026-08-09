// @vitest-environment jsdom
//
// The repo picker has to keep two failures apart (SIGMA-237).
//
// "The org has never installed the GitHub App" and "the control plane did not
// answer this request" both used to arrive at the picker as an empty list, and
// the picker answered both by calling onUnavailable() — which is what raises the
// Connect-GitHub panel in SourceStep. That panel's primary button sends the user
// to github.com to re-run an App installation on their organization. Offering it
// during a thirty-second CP redeploy asks a user to change org-wide repository
// access to fix an outage that would have cleared itself.
//
// So: a transport failure must produce a retry, never the install offer. Only a
// control plane that answered, and answered "not connected" (or "connected, but
// nothing granted"), may raise it.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listGitRepos = vi.fn();
vi.mock("@/server/actions/git", () => ({ listGitRepos: (orgId: string) => listGitRepos(orgId) }));

import { RepoPicker } from "./repo-picker";
import { SourceStep } from "./wizard/source-step";

afterEach(() => {
  cleanup();
  listGitRepos.mockReset();
});

describe("RepoPicker", () => {
  it("a control-plane failure shows a retry, not the GitHub install offer", async () => {
    const user = userEvent.setup();
    const onUnavailable = vi.fn();
    listGitRepos.mockResolvedValueOnce({
      repos: [],
      connected: false,
      error: "Couldn't reach the control plane to list your repositories: fetch failed.",
    });

    render(<RepoPicker orgId="org_1" onSelect={() => {}} onUnavailable={onUnavailable} />);

    // The failure is named, and it is named as OURS, not as the user's missing
    // integration.
    expect(await screen.findByText(/couldn’t reach the control plane/i)).toBeTruthy();
    // onUnavailable is the switch that raises the install panel. It must not
    // have been thrown by a transport failure.
    expect(onUnavailable).not.toHaveBeenCalled();

    // And Retry actually re-asks — with a real answer the list appears.
    listGitRepos.mockResolvedValueOnce({
      repos: [{ fullName: "acme/api", private: true, defaultBranch: "main" }],
      connected: true,
    });
    await user.click(screen.getByRole("button", { name: /retry/i }));
    await waitFor(() => expect(listGitRepos).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("acme/api")).toBeTruthy();
    expect(onUnavailable).not.toHaveBeenCalled();
  });

  it("a thrown action (a redacted server-action digest) is also a retry", async () => {
    const onUnavailable = vi.fn();
    listGitRepos.mockRejectedValueOnce(new Error("Digest: 3128374"));

    render(<RepoPicker orgId="org_1" onSelect={() => {}} onUnavailable={onUnavailable} />);

    expect(await screen.findByRole("button", { name: /retry/i })).toBeTruthy();
    expect(onUnavailable).not.toHaveBeenCalled();
  });

  it("a control plane that answers 'not connected' still raises the install offer", async () => {
    const onUnavailable = vi.fn();
    listGitRepos.mockResolvedValueOnce({ repos: [], connected: false });

    render(<RepoPicker orgId="org_1" onSelect={() => {}} onUnavailable={onUnavailable} />);

    await waitFor(() => expect(onUnavailable).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("button", { name: /retry/i })).toBeNull();
  });
});

describe("SourceStep", () => {
  function renderSourceStep() {
    return render(
      <SourceStep
        cpMode
        orgId="org_1"
        repo={null}
        branch=""
        onPickRepo={() => {}}
        onBranchChange={() => {}}
        detecting={false}
        gitAppSlug="sigmahub"
        installUrlTarget={{ kind: "wizard" }}
        onBeforeLeaveForGitHub={() => {}}
        manualRepo=""
        onManualRepoChange={() => {}}
        token=""
        onTokenChange={() => {}}
        onDetectManual={() => {}}
        detectError={null}
      />
    );
  }

  it("does not offer the GitHub App install when the control plane is unreachable", async () => {
    listGitRepos.mockResolvedValueOnce({
      repos: [],
      connected: false,
      error: "Couldn't reach the control plane to list your repositories: fetch failed.",
    });

    renderSourceStep();

    expect(await screen.findByRole("button", { name: /retry/i })).toBeTruthy();
    // The install CTA — the thing that sends people to github.com to re-run an
    // org-wide installation — must be absent.
    expect(screen.queryByText(/Install the sigmahub app once/i)).toBeNull();
    expect(screen.queryByRole("link", { name: /connect github/i })).toBeNull();
  });

  it("still offers the install when the org genuinely has no integration", async () => {
    listGitRepos.mockResolvedValueOnce({ repos: [], connected: false });

    renderSourceStep();

    expect(await screen.findByText(/Install the sigmahub app once/i)).toBeTruthy();
  });
});
