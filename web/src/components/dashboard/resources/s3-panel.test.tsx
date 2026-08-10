// @vitest-environment jsdom
//
// Deleting a bucket destroys the bucket and every object in it, and it used to
// fire straight off a bare trash icon — one click, no dialog, nothing named
// (SIGMA-311). These tests hold the panel to the same bar the rest of the
// product sets for irreversible work: a second, explicit step that says what
// is about to be destroyed.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

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
vi.mock("@/server/actions/s3", () => ({
  revealS3Connection: vi.fn(),
  listBuckets: vi.fn(),
  createBucket: vi.fn(),
  deleteBucket: vi.fn(),
  createBucketKey: vi.fn(),
}));

import { deleteBucket, listBuckets } from "@/server/actions/s3";
import { S3Panel } from "./s3-panel";
import type { CpBucket, CpS3Info } from "@/server/cp";

const INFO: CpS3Info = {
  resourceId: "res_1",
  engine: "minio",
  image: "minio/minio:RELEASE",
  accessKey: "root-access-key",
  host: "10.0.0.4",
  port: 9000,
  meshOnly: true,
  endpoint: "http://10.0.0.4:9000",
};

const UPLOADS: CpBucket = {
  id: "bkt_1",
  resourceId: "res_1",
  name: "uploads",
  quotaBytes: 0,
  accessKey: "",
  status: "active",
};

function renderPanel(buckets: CpBucket[] = [UPLOADS]) {
  vi.mocked(listBuckets).mockResolvedValue(buckets);
  return render(
    <S3Panel orgId="org_1" resourceId="res_1" info={INFO} canManage />
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("S3Panel bucket delete", () => {
  it("deleting a bucket requires confirmation naming the bucket and its objects", async () => {
    renderPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Delete uploads" }));

    // The trash icon opens a dialog; it is not itself the deletion.
    expect(deleteBucket).not.toHaveBeenCalled();

    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain("uploads");
    expect(dialog.textContent).toMatch(/everything in it/i);

    fireEvent.click(screen.getByRole("button", { name: /^Delete bucket$/ }));
    expect(deleteBucket).toHaveBeenCalledWith({
      orgId: "org_1",
      resourceId: "res_1",
      bucket: "uploads",
    });
  });

  it("cancelling the confirmation deletes nothing", async () => {
    renderPanel();
    fireEvent.click(await screen.findByRole("button", { name: "Delete uploads" }));
    fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
    expect(deleteBucket).not.toHaveBeenCalled();
  });
});
