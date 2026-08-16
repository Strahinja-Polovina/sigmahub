// How a server action says no (SIGMA-365).
//
// Next.js REDACTS errors thrown out of a server action in a production build.
// The client receives a generic message and a digest; the sentence the action
// wrote is server-side only. So every `throw new Error("That person is already
// a member.")` in this codebase is a message the user sees in `next dev` and
// never sees in production — where instead they get "An unexpected error
// occurred", on a refusal that was supposed to tell them what to do next.
//
// That is not a cosmetic problem. Almost every one of these messages exists to
// name an EXIT: "resend or revoke it first", "point the policy elsewhere first",
// "sign in with that email to accept it", "copy the invite link and send it
// yourself". Redacted, each one becomes the dead end the message was written to
// prevent — which is the single defect class this review series keeps finding.
//
// The codebase had already hit this twice and patched around it locally: git.ts
// and the install-command actions return `{ ok: false, error }` by hand, and
// install-command.test.ts explains why in a comment. This makes that the rule
// instead of the exception.
//
// The redaction is not arbitrary, though, and this must not simply undo it: it
// exists so an unexpected failure — a driver error, a constraint name, a
// timeout carrying a connection string — is not handed to whoever triggered it.
// So the two cases are separated. A `Refusal` is a sentence the action chose to
// show a human, and is forwarded verbatim. Anything else is logged server-side
// and reaches the client as a generic apology, exactly as Next intended.

/**
 * A refusal the user is meant to read. Throw this — not `Error` — for anything a
 * person can act on: a permission check, a validation failure, a limit, a
 * conflict, a precondition. The message crosses to the browser verbatim, so it
 * must be a complete sentence aimed at a human and must not carry identifiers,
 * SQL, or anything about the deployment's internals.
 */
export class Refusal extends Error {
  constructor(message: string) {
    super(message);
    this.name = "Refusal";
  }
}

/** What every server action returns. Flattened rather than `{ ok, data }` to
 *  match the shape git.ts and the install-command actions already use, so the
 *  call sites that were written before this file do not have to change twice. */
export type ActionResult<T> = ({ ok: true } & T) | { ok: false; error: string };

/** What the client is told when the failure was not a deliberate refusal. */
export const GENERIC_FAILURE =
  "Something went wrong. Please try again, or contact your administrator if it keeps happening.";

/**
 * Run an action body and turn its outcome into an ActionResult.
 *
 * Wrapping is deliberately the ONLY change an action needs: the `throw` style
 * inside the body stays exactly as it was, so converting a file is a
 * type-signature edit plus swapping `Error` for `Refusal` on the throws that
 * were always meant for a person.
 */
export async function attempt<T extends object | void>(
  fn: () => Promise<T>
): Promise<ActionResult<T extends void ? object : T>> {
  try {
    const value = await fn();
    return { ok: true, ...(value ?? {}) } as ActionResult<T extends void ? object : T>;
  } catch (err) {
    if (err instanceof Refusal) {
      return { ok: false, error: err.message };
    }
    // Deliberately not forwarded. This is the case Next's redaction is for, and
    // the log line is the operator's copy — without it the failure would be
    // invisible on both sides.
    console.error("[action] unexpected failure:", err);
    return { ok: false, error: GENERIC_FAILURE };
  }
}

/**
 * Client side: take the result and give back the value, throwing on a refusal.
 *
 * This exists so converting a caller is one line. Every dialog in the dashboard
 * already wraps its action call in try/catch and renders `err.message` in a
 * toast; with `unwrap` around the call, that code keeps working unchanged and
 * starts showing the real sentence in production instead of a digest.
 */
export function unwrap<T extends object>(res: ActionResult<T>): T {
  if (!res.ok) throw new Error(res.error);
  const rest: Record<string, unknown> = { ...res };
  delete rest.ok;
  return rest as unknown as T;
}
