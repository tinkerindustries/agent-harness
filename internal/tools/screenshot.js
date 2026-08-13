// The Screenshot tool's browser driver (docs/TOOLS.md, "Screenshot").
// internal/tools/screenshot.go embeds this file, writes it to a temporary
// path and runs it with `node`, passing one JSON configuration object as
// argv[2] and reading one JSON report back from stdout.
//
// This is a script rather than a `playwright screenshot` invocation because
// the CLI cannot do the two things the tool exists to offer: clip the capture
// to an element (the fix for a small control being unreadable after a
// full-page capture is downscaled) and set a device scale factor. Driving the
// library directly also lets the page's console errors come back with the
// image, so a blank screenshot arrives with the reason it was blank rather
// than needing a second call to find out.
//
// The Dockerfile puts playwright in the global module path and points it at
// Alpine's Chromium, so a bare require works with no browser download.
const { chromium } = require("playwright");

async function main() {
  const cfg = JSON.parse(process.argv[2]);

  const browser = await chromium.launch();
  // Everything after the launch is wrapped, so the browser is closed on any
  // failure. A leaked chromium would outlive the tool call and hold its
  // share of the container's memory for the rest of the session.
  try {
    const context = await browser.newContext({
      viewport: { width: cfg.width, height: cfg.height },
      deviceScaleFactor: cfg.scale,
      colorScheme: cfg.colorScheme,
    });
    const page = await context.newPage();

    // Attached before the navigation so an error thrown during initial render
    // is caught. Both lists are capped: a page in a render loop can log
    // thousands of identical errors, and the tool result has a byte cap that
    // they would otherwise consume entirely.
    const consoleErrors = [];
    const pageErrors = [];
    page.on("console", (msg) => {
      if (msg.type() === "error" && consoleErrors.length < 20) consoleErrors.push(msg.text());
    });
    page.on("pageerror", (err) => {
      if (pageErrors.length < 20) pageErrors.push(String(err));
    });

    await page.goto(cfg.url, { waitUntil: "load", timeout: cfg.navigationTimeoutMs });

    // A single-page app has usually not rendered anything at "load", so a
    // capture taken there is a screenshot of an empty root div. Waiting for
    // the network to settle is what makes the common case right. It is
    // allowed to fail: a page holding a websocket or polling on an interval
    // never goes idle, and that is not a reason to fail the capture.
    await page.waitForLoadState("networkidle", { timeout: cfg.settleTimeoutMs }).catch(() => {});

    if (cfg.waitForSelector) {
      await page.waitForSelector(cfg.waitForSelector, { timeout: cfg.navigationTimeoutMs });
    }
    if (cfg.waitMs > 0) {
      await page.waitForTimeout(cfg.waitMs);
    }

    let clipped = false;
    if (cfg.selector) {
      const locator = page.locator(cfg.selector).first();
      // An explicit wait gives "selector not found" as the error rather than
      // whatever screenshot() reports when it resolves to nothing.
      await locator.waitFor({ state: "visible", timeout: cfg.navigationTimeoutMs });
      await locator.screenshot({ path: cfg.path });
      clipped = true;
    } else {
      await page.screenshot({ path: cfg.path, fullPage: cfg.fullPage });
    }

    const dimensions = await page.evaluate(() => ({
      documentHeight: document.documentElement.scrollHeight,
      viewportHeight: window.innerHeight,
    }));

    process.stdout.write(
      JSON.stringify({
        ok: true,
        url: page.url(),
        title: await page.title(),
        clipped,
        documentHeight: dimensions.documentHeight,
        viewportHeight: dimensions.viewportHeight,
        consoleErrors,
        pageErrors,
      }),
    );
  } finally {
    await browser.close();
  }
}

main().catch((err) => {
  // The message only — a Playwright stack trace is a page of module paths
  // that says nothing the model can act on.
  process.stdout.write(JSON.stringify({ ok: false, error: err && err.message ? err.message : String(err) }));
  process.exitCode = 1;
});
