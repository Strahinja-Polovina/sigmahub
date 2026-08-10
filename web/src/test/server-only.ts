/**
 * Test stub for the `server-only` package.
 *
 * Server modules start with `import "server-only"` — a Next.js build-time guard
 * that makes a client bundle fail loudly if it ever pulls one in. It has no
 * runtime shape at all, and it is not installed as a dependency (Next resolves
 * it internally), so a component test that renders a client component whose
 * action module imports it cannot resolve the specifier. Vitest aliases
 * `server-only` here instead, which keeps the guard exactly where it belongs —
 * in the real build — while letting tests import the module graph.
 */
export {};
