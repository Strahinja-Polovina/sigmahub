// @vitest-environment jsdom
//
// Deleting a project cascades every environment and every resource inside it
// (the CP's DeleteProject → cascadeResourceCleanupTx). The dialog used to be
// prose and one red button, while deleting a SINGLE resource already demands
// the resource's name be typed — the confirmation bar was inverted with respect
// to blast radius (SIGMA-314).
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
  });
  return { toast };
});
vi.mock("@/server/actions/projects", () => ({
  deleteProject: vi.fn(),
  renameProject: vi.fn(),
}));

import { deleteProject } from "@/server/actions/projects";
import { ProjectCardMenu } from "./project-card-menu";

const RESOURCES = [
  { id: "res_1", name: "api", kind: "app", envName: "production" },
  { id: "res_2", name: "db", kind: "postgres", envName: "production" },
];

async function openDelete(resources = RESOURCES) {
  render(
    <ProjectCardMenu
      projectId="prj_1"
      name="Acme"
      description=""
      envCount={2}
      resources={resources}
    />
  );
  fireEvent.click(screen.getByRole("button", { name: "Project actions" }));
  fireEvent.click(await screen.findByText("Delete"));
  return screen.findByRole("dialog");
}

function confirmButton(): HTMLButtonElement {
  return screen.getByRole("button", { name: /^Delete project$/ }) as HTMLButtonElement;
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ProjectCardMenu delete", () => {
  it("stays disabled until the project's name is typed", async () => {
    await openDelete();
    expect(confirmButton().disabled).toBe(true);

    fireEvent.change(screen.getByLabelText(/type/i), { target: { value: "Acm" } });
    expect(confirmButton().disabled).toBe(true);

    fireEvent.change(screen.getByLabelText(/type/i), { target: { value: "Acme" } });
    expect(confirmButton().disabled).toBe(false);

    fireEvent.click(confirmButton());
    expect(deleteProject).toHaveBeenCalledWith({ projectId: "prj_1" });
  });

  it("itemises the resources the cascade destroys, with a count", async () => {
    const dialog = await openDelete();
    const rows = within(dialog).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]?.textContent).toContain("api");
    expect(rows[1]?.textContent).toContain("postgres");
    expect(dialog.textContent).toMatch(/2 environments/);
    expect(dialog.textContent).toMatch(/2 resources/);
  });
});
