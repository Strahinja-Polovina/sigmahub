import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// This is a source test, not a render test: the web app has no DOM test
// environment (vitest runs with `environment: "node"`), and the property under
// test is a property of the source anyway — whether a menu item's handler
// reaches the server at all. See clusters.test.ts / hosting.test.ts for the
// same technique.
const SOURCE = readFileSync(
  join(process.cwd(), "src/components/dashboard/servers/server-detail-view.tsx"),
  "utf8"
);

/** The end index (exclusive) of the balanced `{...}` expression that starts at
 *  `open` (the index of the `{`). */
function endOfBraced(src: string, open: number): number {
  let depth = 0;
  for (let i = open; i < src.length; i++) {
    if (src[i] === "{") depth++;
    else if (src[i] === "}") {
      depth--;
      if (depth === 0) return i + 1;
    }
  }
  throw new Error("unbalanced braces in JSX attribute");
}

type MenuItem = { label: string; onClick: string };

/** Every <DropdownMenuItem> in the file, with its visible label and the source
 *  text of its onClick expression. */
function menuItems(src: string): MenuItem[] {
  const items: MenuItem[] = [];
  const OPEN = "<DropdownMenuItem";
  const CLOSE = "</DropdownMenuItem>";
  for (let at = src.indexOf(OPEN); at !== -1; at = src.indexOf(OPEN, at + 1)) {
    // Walk the opening tag, skipping over braced attribute values, to find the
    // `>` that ends it.
    let i = at + OPEN.length;
    for (; i < src.length; i++) {
      if (src[i] === "{") i = endOfBraced(src, i) - 1;
      else if (src[i] === ">") break;
    }
    const attrs = src.slice(at, i);
    const close = src.indexOf(CLOSE, i);
    // Nested elements (the icons) go; interpolations stay, so an item whose
    // whole label is a ternary still has something to name it by.
    const label = src
      .slice(i + 1, close)
      .replace(/<[^>]*>/g, "")
      .replace(/\s+/g, " ")
      .trim();

    const clickAt = attrs.indexOf("onClick=");
    let onClick = "";
    if (clickAt !== -1) {
      const brace = attrs.indexOf("{", clickAt);
      onClick = attrs.slice(brace + 1, endOfBraced(attrs, brace) - 1).trim();
    }
    items.push({ label, onClick });
  }
  return items;
}

/** Strip the `() =>` / `async () =>` prefix and any wrapping block braces, so
 *  what is left is what the handler actually does first. */
function handlerBody(onClick: string): string {
  return onClick
    .replace(/^async\s+/, "")
    .replace(/^\([^)]*\)\s*=>\s*/, "")
    .replace(/^\{/, "")
    .trim();
}

describe("the server actions menu", () => {
  const items = menuItems(SOURCE);

  it("has menu items to check", () => {
    // Guards the parser: if this file's JSX shape changes so much that nothing
    // parses, the assertions below would pass vacuously.
    expect(items.length).toBeGreaterThan(2);
    for (const item of items) expect(item.label).not.toBe("");
  });

  // The defect this test exists for (SIGMA-235): "Restart agent" and "Cordon"
  // were menu items whose entire handler was a toast. Nothing was persisted, no
  // CP endpoint was called — and the Cordon toast went further and stated a
  // scheduling guarantee ("No new resources will be scheduled here") that no
  // layer of the system implements. An operator who cordoned a host before
  // kernel maintenance was told the host was drained; the wizard kept offering
  // it (serverIsDeployable filters only incompatible/decommissioning) and the
  // CP kept scheduling onto it.
  it("every server action menu item calls a server action", () => {
    const fake = items.filter((item) => /^toast\b/.test(handlerBody(item.onClick)));
    expect(fake.map((item) => item.label)).toEqual([]);
  });

  // Named separately because the harm is specific: a control that claims a
  // scheduling guarantee is worse than a missing one. Cordon may come back, but
  // only with a `schedulable` flag on the CP server row, exclusion in
  // serverOptions/buildInventory, and a refusal in store.CreateResource — the
  // same three places the `incompatible` gate already covers (SIGMA-197).
  it("offers no cordon control while the control plane has no cordon concept", () => {
    // Labels and handlers, not the whole file: the comment where the removed
    // items used to be names cordon on purpose, so the next reader knows why
    // there is a gap and what filling it costs.
    for (const item of items) {
      expect(item.label).not.toMatch(/cordon/i);
      expect(item.onClick).not.toMatch(/cordon/i);
    }
  });
});
