// Prints what a rendered screen measurably is: every element's box, type,
// colour, contrast and spacing, as numbers.
//
// This exists for the agent sessions that cannot see. The vision path
// (Screenshot -> ReviewScreenshot/AskVision) translates pixels into words well
// enough to say what is on a page, and cannot measure it at all: asked for
// geometry in sess-e78152d6 it put a badge at x=70 that was at x=46, called a
// 28px nav link 40px, a 30px button 48px, and a 5.79:1 badge "barely legible" —
// four numeric claims, four wrong, each stated with the same confidence as the
// true observations around them. A model doing design work off those numbers is
// designing off fiction, and the session's one original proposal came out of
// exactly that gap.
//
// So the division is: Gemini says what the page looks like, this says what it
// is. Neither judges. That is deliberate — there is no threshold anywhere in
// this file, nothing is flagged as too small or too tight, and it always exits
// 0. Its sibling scripts/layout-check.mjs is the one that grades, and it grades
// one thing (horizontal overflow) that is unambiguously a defect. Everything
// here is a judgement someone else has to make, and baking in a rule ("44px
// targets") would be this tool taking the design decision the caller is
// supposed to be taking. Report, do not grade.
//
// Run through scripts/layout-metrics.sh, which drives it inside the harness
// container — that is where Playwright and Chromium already live.
//
// Usage: BASE_URL=http://host.docker.internal:8080 node layout-metrics.mjs <route> [--width=390] [--root=.eval-run] [--scheme=dark]

import pw from "/usr/local/lib/node_modules/playwright/index.js";

const args = process.argv.slice(2);
const flag = (name, fallback) => {
  const hit = args.find((a) => a.startsWith(`--${name}=`));
  return hit ? hit.slice(name.length + 3) : fallback;
};

const base = process.env.BASE_URL ?? "http://localhost:8080";
const route = args.find((a) => !a.startsWith("--")) ?? "/";
const width = Number(flag("width", "1280"));
const height = Number(flag("height", "900"));
const scheme = flag("scheme", "light");
const root = flag("root", "body");
// A screen with a long list runs to thousands of elements, and a readout nobody
// can hold in context is the same as no readout. The repeat collapsing below
// does most of the work; this is the backstop.
const maxNodes = Number(flag("max-nodes", "260"));

const browser = await pw.chromium.launch();
const page = await browser.newPage({ viewport: { width, height }, colorScheme: scheme });
await page.goto(base + route, { waitUntil: "networkidle", timeout: 30_000 });
// The screens fetch and then render; a snapshot taken at networkidle can still
// be the loading state. Same wait layout-check.mjs uses, for the same reason.
await page.waitForTimeout(1500);

const report = await page.evaluate(
  ({ root, maxNodes }) => {
    const rootEl = document.querySelector(root) ?? document.body;

    // ---- colour ----------------------------------------------------------
    // Everything here works in sRGB 0-255 with a separate alpha, because the
    // one question worth answering — what is the contrast — needs the text
    // colour composited onto whatever is actually behind it, and "behind it"
    // is an ancestor walk, not a property.
    const parse = (s) => {
      const m = String(s).match(/rgba?\(([^)]+)\)/);
      if (!m) return null;
      const p = m[1].split(/[\s,/]+/).filter(Boolean).map(Number);
      return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
    };
    const over = (fg, bg) =>
      fg.a >= 1
        ? fg
        : {
            r: fg.r * fg.a + bg.r * (1 - fg.a),
            g: fg.g * fg.a + bg.g * (1 - fg.a),
            b: fg.b * fg.a + bg.b * (1 - fg.a),
            a: 1,
          };
    const lum = (c) => {
      const ch = (v) => {
        const s = v / 255;
        return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
      };
      return 0.2126 * ch(c.r) + 0.7152 * ch(c.g) + 0.0722 * ch(c.b);
    };
    const ratio = (a, b) => {
      const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
      return (x + 0.05) / (y + 0.05);
    };
    // The nearest ancestor that actually paints. A chain of transparent
    // wrappers is the normal case, so stopping at the first parent would report
    // every contrast against nothing.
    const behind = (el) => {
      for (let p = el; p; p = p.parentElement) {
        const c = parse(getComputedStyle(p).backgroundColor);
        if (c && c.a > 0) return c.a >= 1 ? c : over(c, behind(p.parentElement) ?? { r: 255, g: 255, b: 255, a: 1 });
      }
      return { r: 255, g: 255, b: 255, a: 1 };
    };
    const hex = (c) =>
      "#" + [c.r, c.g, c.b].map((v) => Math.round(v).toString(16).padStart(2, "0")).join("");

    // ---- element facts ---------------------------------------------------
    const label = (el) => {
      const cls = String(el.className || "")
        .split(/\s+/)
        .filter(Boolean)
        .slice(0, 2)
        .join(".");
      return el.tagName.toLowerCase() + (cls ? "." + cls : "");
    };
    // Only text this element owns. Without this every ancestor reports its
    // subtree's text and the whole tree looks like one giant label.
    const ownText = (el) =>
      [...el.childNodes]
        .filter((n) => n.nodeType === 3)
        .map((n) => n.textContent.trim())
        .join(" ")
        .replace(/\s+/g, " ")
        .trim();

    const px = (v) => Math.round(parseFloat(v) || 0);
    const nodes = [];
    let truncated = false;

    const walk = (el, depth) => {
      if (nodes.length >= maxNodes) {
        truncated = true;
        return;
      }
      const cs = getComputedStyle(el);
      const r = el.getBoundingClientRect();
      if (cs.display === "none" || cs.visibility === "hidden" || r.width < 1 || r.height < 1) return;

      const text = ownText(el);
      const node = {
        depth,
        label: label(el),
        x: Math.round(r.x),
        y: Math.round(r.y),
        w: Math.round(r.width),
        h: Math.round(r.height),
        text: text.length > 42 ? text.slice(0, 41) + "…" : text,
        display: cs.display,
      };

      const pad = [cs.paddingTop, cs.paddingRight, cs.paddingBottom, cs.paddingLeft].map(px);
      if (pad.some((v) => v)) node.pad = pad;
      const gap = px(cs.rowGap === "normal" ? 0 : cs.rowGap);
      const colGap = px(cs.columnGap === "normal" ? 0 : cs.columnGap);
      if (gap || colGap) node.gap = [gap, colGap];
      if (cs.overflowX === "auto" || cs.overflowX === "scroll") node.scrolls = true;

      if (text) {
        const fg = parse(cs.color) ?? { r: 0, g: 0, b: 0, a: 1 };
        const bg = behind(el);
        node.font = `${px(cs.fontSize)}px/${cs.lineHeight === "normal" ? "n" : px(cs.lineHeight)} ${cs.fontWeight}`;
        node.mono = /mono/i.test(cs.fontFamily);
        node.fg = hex(over(fg, bg));
        node.bg = hex(bg);
        node.contrast = Math.round(ratio(over(fg, bg), bg) * 10) / 10;
        if (px(cs.letterSpacing)) node.tracking = cs.letterSpacing;
        if (cs.textTransform !== "none") node.transform = cs.textTransform;
        if (cs.textAlign !== "start" && cs.textAlign !== "left") node.align = cs.textAlign;
        // Two different clips, and a design pass wants to know which: a string
        // cut on one line, or a paragraph cut after N lines.
        if (el.scrollWidth > el.clientWidth + 1 && cs.overflowX === "hidden") node.clippedX = el.scrollWidth - el.clientWidth;
        if (el.scrollHeight > el.clientHeight + 1 && cs.overflowY === "hidden") node.clippedY = el.scrollHeight - el.clientHeight;
      }

      nodes.push(node);

      // Collapse runs of same-shaped siblings. A 24-row table is 24 identical
      // subtrees; printing them all buries the page's structure in its data,
      // and the only thing worth knowing about the tail is whether the rows
      // are the same height as each other.
      const kids = [...el.children];
      const sig = (k) => label(k);
      let i = 0;
      while (i < kids.length) {
        let j = i;
        while (j + 1 < kids.length && sig(kids[j + 1]) === sig(kids[i])) j++;
        const run = j - i + 1;
        if (run > 3) {
          walk(kids[i], depth + 1);
          walk(kids[i + 1], depth + 1);
          const rest = kids.slice(i + 2, j + 1).map((k) => Math.round(k.getBoundingClientRect().height));
          nodes.push({
            depth: depth + 1,
            repeat: { label: sig(kids[i]), count: run - 2, min: Math.min(...rest), max: Math.max(...rest) },
          });
        } else {
          for (let k = i; k <= j; k++) walk(kids[k], depth + 1);
        }
        i = j + 1;
      }
    };

    walk(rootEl, 0);

    // ---- vertical rhythm -------------------------------------------------
    // The gaps between consecutive block siblings, which is where an
    // inconsistent spacing scale shows up and where no single element's
    // padding would reveal it.
    const rhythm = [];
    // Bounded depth on purpose: the spacing decisions a design pass argues
    // about are the ones between a screen's bands and their immediate parts,
    // not the gap between two spans inside a badge.
    const rhythmWalk = (el, depth) => {
      if (depth > 4) return;
      const kids = [...el.children].filter((k) => {
        const cs = getComputedStyle(k);
        const r = k.getBoundingClientRect();
        return cs.display !== "none" && r.height >= 1 && cs.position !== "absolute" && cs.position !== "fixed";
      });
      for (let i = 1; i < kids.length; i++) {
        const a = kids[i - 1].getBoundingClientRect();
        const b = kids[i].getBoundingClientRect();
        // Side-by-side items share a row; their vertical gap means nothing.
        if (b.top < a.bottom - 1) continue;
        rhythm.push({ parent: label(el), from: label(kids[i - 1]), to: label(kids[i]), gap: Math.round(b.top - a.bottom) });
      }
      for (const k of kids) rhythmWalk(k, depth + 1);
    };
    rhythmWalk(rootEl, 0);

    return {
      title: document.title,
      docHeight: document.documentElement.scrollHeight,
      rootBox: (() => {
        const r = rootEl.getBoundingClientRect();
        return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) };
      })(),
      nodes,
      rhythm,
      truncated,
    };
  },
  { root, maxNodes },
);

await browser.close();

// ---- formatting ----------------------------------------------------------
// One line per element, fixed columns, so a reader scanning down the page can
// see alignment (the x column) and rhythm (the y column) without doing
// arithmetic — which is the whole point of handing this to something blind.
const out = [];
out.push(`${base}${route}  ${width}x${height} ${scheme}  "${report.title}"  document ${report.docHeight}px tall`);
out.push(`root ${root}  ${report.rootBox.w}x${report.rootBox.h} at ${report.rootBox.x},${report.rootBox.y}`);
out.push("");
out.push("box = x,y wxh    text colour on background, contrast ratio");
out.push("");

for (const n of report.nodes) {
  if (n.repeat) {
    out.push(
      `${"  ".repeat(n.depth)}… ${n.repeat.count} more ${n.repeat.label}, heights ${n.repeat.min}–${n.repeat.max}px`,
    );
    continue;
  }
  const bits = [];
  if (n.pad) bits.push(`pad ${n.pad.join("/")}`);
  if (n.gap) bits.push(`gap ${n.gap[0] === n.gap[1] ? n.gap[0] : n.gap.join("/")}`);
  if (n.scrolls) bits.push("scrolls-x");
  if (n.font) bits.push(`${n.font}${n.mono ? " mono" : ""}`);
  if (n.transform) bits.push(n.transform);
  if (n.tracking) bits.push(`tracking ${n.tracking}`);
  if (n.align) bits.push(`align ${n.align}`);
  if (n.fg) bits.push(`${n.fg} on ${n.bg} ${n.contrast}:1`);
  if (n.clippedX) bits.push(`CLIPPED-X ${n.clippedX}px hidden`);
  if (n.clippedY) bits.push(`CLIPPED-Y ${n.clippedY}px hidden`);
  const box = `${n.x},${n.y} ${n.w}x${n.h}`;
  const head = `${"  ".repeat(n.depth)}${n.label}`;
  out.push(`${head.padEnd(46)}${box.padEnd(20)}${bits.join("  ")}`);
  if (n.text) out.push(`${" ".repeat(n.depth * 2 + 2)}"${n.text}"`);
}

if (report.truncated) out.push(`\n(stopped at --max-nodes=${maxNodes}; pass --root to narrow, or raise the cap)`);

// The rhythm table repeats information already in the boxes, and repeats it in
// the form the question is actually asked in: "is the spacing consistent?"
out.push("");
out.push(`vertical gaps between siblings (${report.rhythm.length})`);
const byGap = new Map();
for (const r of report.rhythm) {
  const k = `${r.parent} ${r.gap}`;
  if (!byGap.has(k)) byGap.set(k, { ...r, n: 0, pairs: [] });
  const e = byGap.get(k);
  e.n++;
  if (e.pairs.length < 2) e.pairs.push(`${r.from}→${r.to}`);
}
for (const e of [...byGap.values()].sort((a, b) => b.n - a.n || b.gap - a.gap)) {
  out.push(`  ${String(e.gap + "px").padEnd(7)} in ${e.parent.padEnd(26)} ${e.n}×  ${e.pairs.join(", ")}`);
}

console.log(out.join("\n"));
