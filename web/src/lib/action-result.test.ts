// The contract that decides whether a refusal reaches the person it was written
// for (SIGMA-365).
//
// Next redacts errors thrown out of a server action in production, so the
// sentences these actions carefully compose — almost all of which name an EXIT
// ("resend or revoke it first", "copy the invite link and send it yourself") —
// arrive as "an unexpected error occurred" plus a digest. Each one then becomes
// the dead end it was written to prevent.
//
// The fix must not simply undo the redaction: it exists so a driver error or a
// constraint name is not handed to whoever tripped it. So the whole property is
// the SPLIT, and that is what is pinned here.

import { describe, expect, it, vi } from "vitest";

import { GENERIC_FAILURE, Refusal, attempt, unwrap } from "./action-result";

describe("attempt", () => {
  it("carries a Refusal's sentence through verbatim", async () => {
    const res = await attempt(async () => {
      throw new Refusal("An invite is already pending for that email — resend or revoke it first.");
    });
    expect(res.ok).toBe(false);
    expect(res.ok ? "" : res.error).toBe(
      "An invite is already pending for that email — resend or revoke it first."
    );
  });

  it("does NOT carry an unexpected error's message, and logs it instead", async () => {
    // The reason Next redacts. A driver error can carry a constraint name, a
    // query fragment, or a connection string, and the person who tripped it is
    // not necessarily someone we want holding any of that.
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const res = await attempt(async () => {
      throw new Error('duplicate key value violates unique constraint "servers_pkey" host=10.0.0.4');
    });
    expect(res.ok).toBe(false);
    const shown = res.ok ? "" : res.error;
    expect(shown).toBe(GENERIC_FAILURE);
    expect(shown).not.toMatch(/constraint|10\.0\.0\.4/);
    // ...but the operator still gets it. Redacting on BOTH sides would be worse
    // than either alternative: nobody would know anything happened.
    expect(spy).toHaveBeenCalled();
    spy.mockRestore();
  });

  it("returns the action's value flattened alongside ok", async () => {
    const res = await attempt(async () => ({ inviteUrl: "https://x/invite/t", delivered: true }));
    expect(res).toEqual({ ok: true, inviteUrl: "https://x/invite/t", delivered: true });
  });

  it("handles an action that returns nothing", async () => {
    const res = await attempt(async () => {});
    expect(res).toEqual({ ok: true });
  });

  it("treats a subclass of Refusal as a refusal", async () => {
    class BudgetExceeded extends Refusal {}
    const res = await attempt(async () => {
      throw new BudgetExceeded("You have sent 25 invites in the last hour.");
    });
    expect(res.ok ? "" : res.error).toMatch(/25 invites/);
  });
});

describe("unwrap", () => {
  it("gives back the value without the ok flag", () => {
    expect(unwrap({ ok: true, delivered: false, inviteUrl: "u" })).toEqual({
      delivered: false,
      inviteUrl: "u",
    });
  });

  it("throws the refusal's sentence, so an existing try/catch keeps working", () => {
    // Every dialog already does `catch (err) { toast(err.message) }`. That code
    // must not have to change — only the call it wraps.
    expect(() => unwrap({ ok: false, error: "That person is already a member." })).toThrow(
      "That person is already a member."
    );
  });
});
