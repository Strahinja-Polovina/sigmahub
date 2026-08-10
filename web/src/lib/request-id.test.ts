import { describe, expect, it } from "vitest";

import { createSubmissionId } from "./request-id";

describe("createSubmissionId", () => {
  it("hands a retry of the same submission the same id", () => {
    const sub = createSubmissionId();
    const a = sub.forContent("prod|env_x|srv_1");
    const b = sub.forContent("prod|env_x|srv_1");
    expect(a).toBe(b);
  });

  it("mints a new id once the submission has landed", () => {
    // The operator created `prod`, the k3s install failed, they deleted it and
    // built it again. Byte-identical content, and a genuinely new submission —
    // reusing the first id replays the deleted cluster's 201 (SIGMA-253).
    const sub = createSubmissionId();
    const first = sub.forContent("prod|env_x|srv_1");
    sub.settled();
    expect(sub.forContent("prod|env_x|srv_1")).not.toBe(first);
  });

  it("mints a new id when the content changes", () => {
    // The control plane answers 409 for a key reused with a different body, so
    // an edited form must not carry the failed attempt's key.
    const sub = createSubmissionId();
    const first = sub.forContent("prod|env_x|srv_1");
    expect(sub.forContent("prod|env_x|srv_2")).not.toBe(first);
  });

  it("does not go back to a previous submission's id", () => {
    const sub = createSubmissionId();
    const first = sub.forContent("a");
    sub.forContent("b");
    expect(sub.forContent("a")).not.toBe(first);
  });
});
