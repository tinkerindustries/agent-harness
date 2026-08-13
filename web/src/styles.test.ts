import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

// The file is read rather than imported. Vite's `?raw` returns an empty
// string for a CSS file under vitest, which made an earlier version of this
// guard pass by having nothing to check — the exact failure it exists to
// prevent, so the first assertion below is that the source arrived at all.

const css = readFileSync(fileURLToPath(new URL("./styles.css", import.meta.url)), "utf8");

// An undefined custom property fails silently. `var(--space-6)` where nothing
// defines --space-6 renders as nothing: no build error, no console warning,
// no failing test — the rule simply does not apply. That produced a facts row
// with no gaps and section headings with no margin, and neither the
// typechecker nor the 255 tests beside this one could have caught it.
//
// The spacing scale in particular is --space, --space-half, --space-2,
// --space-3, --space-4. There is no --space-1, --space-5, --space-6 or
// --space-7, which is exactly the mistake this exists to catch.

// used counts the first identifier inside each var(), ignoring the fallback:
// var(--a, var(--b)) references both, and the outer match plus the inner one
// catch each in turn.
function used(source: string): Set<string> {
  return new Set([...source.matchAll(/var\(\s*(--[a-zA-Z0-9-]+)/g)].map((m) => m[1]));
}

function defined(source: string): Set<string> {
  return new Set([...source.matchAll(/^\s*(--[a-zA-Z0-9-]+)\s*:/gm)].map((m) => m[1]));
}

describe("styles.css", () => {
  // A guard with nothing to check passes for ever. This is the assertion
  // that stops that.
  it("was read", () => {
    expect(css.length).toBeGreaterThan(1000);
    expect(used(css).size).toBeGreaterThan(20);
  });

  it("defines every custom property it uses", () => {
    const missing = [...used(css)].filter((name) => !defined(css).has(name)).sort();
    expect(missing, `undefined custom properties: ${missing.join(", ")}`).toEqual([]);
  });

  // The guard is only worth having if it would actually fire.
  it("would catch an undefined property", () => {
    const broken = ".x { padding: var(--space-6); }";
    expect([...used(broken)].filter((n) => !defined(css).has(n))).toEqual(["--space-6"]);
  });

  // Every screen sits inside .screen, which owns the page width and gutters.
  // A screen that rolls its own padding runs edge to edge under an inset nav.
  it("defines the shared page container", () => {
    expect(css).toMatch(/^\.screen\s*\{/m);
  });
});
