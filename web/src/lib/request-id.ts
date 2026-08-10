/**
 * Request ids for `Idempotency-Key`.
 *
 * The header means one thing: "this is the same submission as the one I just
 * sent". The control plane holds up its end — a matching key with a matching
 * body replays the stored response instead of re-executing the mutation
 * (cp/internal/api/idempotency.go) — but only the CLIENT knows where one
 * submission ends and the next begins, so only the client can mint the key.
 *
 * Getting that wrong has two failure modes and the app managed both:
 *
 *   - a key derived from the request's CONTENT (SIGMA-253) never changes while
 *     the content does not, so a genuinely new submission replays an old
 *     response. Delete a cluster and rebuild it under the same name and the
 *     201 you get back names the deleted cluster.
 *   - a key minted per CALL (SIGMA-256) is never repeated, so the claim always
 *     wins and every retry re-executes: a second audit entry, a second round of
 *     config deployments, a second restart wave across every consumer — during
 *     what the user believes was a failed operation.
 *
 * So an id is scoped to a submission, and a submission is "this content, until
 * it lands". Content is part of it because the control plane answers 409 when a
 * key is reused with a different body: an operator who fixes a typo after a
 * failure is making a NEW submission, not retrying the old one.
 */

/** A fresh request id. Opaque to the control plane, which only compares it. */
export function newRequestId(): string {
  // randomUUID is available in every browser context this app runs in (secure
  // origins and localhost) and in Node; the fallback exists so a non-secure
  // origin degrades to "no deduplication" rather than to a thrown TypeError in
  // the middle of a submit handler.
  const c = globalThis.crypto;
  if (c && typeof c.randomUUID === "function") return c.randomUUID();
  return `req_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
}

export type SubmissionId = {
  /** The id for a submission carrying this content. The same content gets the
   *  same id until `settled()` is called, which is exactly what makes a retry
   *  a retry. */
  forContent(signature: string): string;
  /** This submission reached a definitive outcome; whatever comes next is a new
   *  intent and gets a new id, even if the operator types the same thing. */
  settled(): void;
};

/** One of these per form. Keep it in a ref — it is state, not a computation. */
export function createSubmissionId(): SubmissionId {
  let current: { signature: string; id: string } | null = null;
  return {
    forContent(signature) {
      if (!current || current.signature !== signature) {
        current = { signature, id: newRequestId() };
      }
      return current.id;
    },
    settled() {
      current = null;
    },
  };
}
