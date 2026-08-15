// Ceilings on organizations themselves (SIGMA-365).
//
// Lives in lib/ rather than beside the action because a `"use server"` module may
// only export async functions — a plain `export const` there makes Next drop
// every export from the module, and the build fails with "the module has no
// exports at all" pointing at the importers rather than the cause.

/**
 * How many organizations one account may administer.
 *
 * Every abuse limit added for the public launch is scoped PER ORG — the free
 * tier bounds units per org, the outbound-mail budget bounds invite email per
 * org — and createOrg needs nothing but a session. So without this, a loop is
 * unlimited free capacity and an unlimited mail cannon at once: exactly the two
 * things those limits exist to prevent, bypassed by the object they are counted
 * against.
 *
 * Well above the consultant createOrg was built for (SIGMA-306: several client
 * fleets kept apart) and far below anything worth farming. Someone who
 * legitimately reaches it is a conversation, not a loop.
 */
export const MAX_ORGS_PER_USER = 10;
