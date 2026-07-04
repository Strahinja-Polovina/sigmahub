/** Generate a prefixed random ID (e.g. `proj_a1b2c3d4e5f6`). */
export function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

/** Slugify a name for URL-safe usage. */
export function slugify(x: string) {
  return (
    x
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 40) || "project"
  );
}
