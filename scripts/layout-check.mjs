// Loads every screen at two widths and fails on horizontal overflow.
//
// Nothing else catches this. jsdom has no layout engine, so a component test
// cannot see geometry; the typechecker cannot see CSS at all. A table with no
// scroll container took the eval list to 557px inside a 390px viewport, and
// only a browser could say so.
//
// Run through scripts/layout-check.sh, which drives it inside the harness
// container — that is where Playwright and Chromium already live.

import pw from "/usr/local/lib/node_modules/playwright/index.js";

const base = process.env.BASE_URL ?? "http://localhost:8080";

// Every route the UI serves. An id-bearing route is filled in below from
// whatever the running stack actually has, so the check covers the pages a
// list cannot reach on its own.
const staticRoutes = ["/", "/evals", "/operations", "/settings"];

const viewports = [
  { name: "desktop", width: 1280, height: 900 },
  { name: "phone", width: 390, height: 844 },
];

async function firstID(path, pick) {
  try {
    const res = await fetch(base + path);
    if (!res.ok) return null;
    const rows = await res.json();
    return Array.isArray(rows) && rows.length > 0 ? pick(rows[0]) : null;
  } catch {
    return null;
  }
}

const routes = [...staticRoutes];
const sessionID = await firstID("/api/sessions", (r) => r.id);
if (sessionID) routes.push(`/sessions/${sessionID}`);
const evalID = await firstID("/api/evals", (r) => r.id);
if (evalID) routes.push(`/evals/${evalID}`);

const browser = await pw.chromium.launch();
const failures = [];

for (const viewport of viewports) {
  for (const route of routes) {
    const page = await browser.newPage({ viewport: { width: viewport.width, height: viewport.height } });
    const consoleErrors = [];
    page.on("console", (m) => m.type() === "error" && consoleErrors.push(m.text()));
    try {
      await page.goto(base + route, { waitUntil: "networkidle", timeout: 30_000 });
      // The screens fetch and then render; a snapshot taken at networkidle can
      // still be the loading state.
      await page.waitForTimeout(1500);

      const wide = await page.evaluate(() => {
        // document.scrollWidth is not the measure. An ancestor that clips
        // reports no document scroll while a table still runs off the side
        // of the screen, unreachable — which is how a 553px table inside a
        // 390px viewport passed a scrollWidth check.
        //
        // So: walk the elements and report any whose right edge is past the
        // viewport, skipping anything inside a container that scrolls on
        // purpose. Content inside an overflow-x: auto box is meant to be
        // wider than its box; that is the fix, not the defect.
        const scrolls = (el) => {
          const o = getComputedStyle(el).overflowX;
          return o === "auto" || o === "scroll";
        };
        const inAScroller = (el) => {
          for (let p = el.parentElement; p; p = p.parentElement) {
            if (scrolls(p)) return true;
          }
          return false;
        };
        const out = [];
        for (const el of document.body.querySelectorAll("*")) {
          const rect = el.getBoundingClientRect();
          if (rect.width === 0 || rect.height === 0) continue;
          const past = Math.round(rect.right - window.innerWidth);
          if (past <= 1 || inAScroller(el)) continue;
          out.push({
            selector: el.tagName.toLowerCase() + (el.className ? "." + String(el.className).split(/\s+/)[0] : ""),
            past,
          });
        }
        // The outermost offender is the useful one: its children repeat it.
        return out.sort((a, b) => b.past - a.past).slice(0, 3);
      });
      const status = wide.length > 0 ? `OVERFLOW: ${wide.map((w) => `${w.selector} +${w.past}px`).join(", ")}` : "ok";
      console.log(`${viewport.name.padEnd(8)} ${route.padEnd(46)} ${status}`);
      for (const w of wide) {
        failures.push(`${route} at ${viewport.width}px: ${w.selector} runs ${w.past}px past the viewport`);
      }
      for (const err of consoleErrors) {
        failures.push(`${route} at ${viewport.width}px logged a console error: ${err}`);
      }
    } catch (err) {
      failures.push(`${route} at ${viewport.width}px did not load: ${err.message}`);
    } finally {
      await page.close();
    }
  }
}

await browser.close();

if (failures.length > 0) {
  console.error(`\n${failures.length} problem(s):`);
  for (const f of failures) console.error(`  ${f}`);
  process.exit(1);
}
console.log(`\nnothing past the viewport and no console errors across ${routes.length} routes`);
