// @vitest-environment jsdom
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn(), push: vi.fn(), replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
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
  createResource: vi.fn(),
  deployResource: vi.fn(),
}));
vi.mock("@/server/actions/secrets", () => ({ createSecretAction: vi.fn() }));
vi.mock("@/server/actions/git", () => ({
  detectRepo: vi.fn(),
  getGitAppInfo: vi.fn(),
  wireRepoToEnvironment: vi.fn(),
  listGitRepos: vi.fn(),
}));
vi.mock("@/server/actions/databases", () => ({ revealDatabaseConnection: vi.fn() }));
vi.mock("@/server/actions/s3", () => ({ revealS3Connection: vi.fn() }));
vi.mock("@/server/actions/llm", () => ({ resolveModel: vi.fn(), searchModels: vi.fn() }));

import { ResourcesView } from "./resources-view";

const resources = [
  {
    id: "res_1",
    name: "api",
    kind: "app",
    status: "running",
    projectName: "Acme",
    envName: "production",
    environmentId: "env_1",
    lastDeployAt: new Date("2026-08-01T10:00:00Z"),
    domain: "app.example.com",
  },
  {
    id: "res_2",
    name: "pg",
    kind: "postgres",
    status: "running",
    projectName: "Acme",
    envName: "production",
    environmentId: "env_1",
    lastDeployAt: new Date("2026-08-01T10:00:00Z"),
    domain: null,
  },
];

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function openRowMenu(name: string) {
  fireEvent.click(screen.getByRole("button", { name: `Actions for ${name}` }));
}

describe("ResourcesView row actions", () => {
  it("the Open menu item is an anchor to the resource's domain", () => {
    render(<ResourcesView orgName="Acme" resources={resources} targets={[]} />);
    openRowMenu("api");

    const open = screen.getByRole("menuitem", { name: /^Open$/ });
    expect(open.tagName).toBe("A");
    expect(open.getAttribute("href")).toBe("https://app.example.com");
    expect(open.getAttribute("target")).toBe("_blank");
    expect(open.getAttribute("rel")).toContain("noopener");
  });

  it("a resource with no domain has nothing to open", () => {
    render(<ResourcesView orgName="Acme" resources={resources} targets={[]} />);
    openRowMenu("pg");
    expect(screen.queryByRole("menuitem", { name: /^Open$/ })).toBeNull();
  });
});
