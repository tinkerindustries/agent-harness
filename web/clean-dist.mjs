// Empties the build output before a build, keeping .gitkeep.
//
// `npm run build` is `tsc -b && vite build`. When the typecheck fails vite
// never runs, and without this the previous build stays on disk looking
// perfectly healthy — the Go binary embeds it, the container serves it, and
// a browser shows a version of the app that no longer matches the source.
// That is a worse failure than no assets at all, because it is silent.
//
// .gitkeep survives: internal/webassets/embed.go embeds this directory, and
// go:embed fails to compile on one with nothing in it.

import { readdirSync, rmSync } from "node:fs";
import { join } from "node:path";

const dist = new URL("../internal/webassets/dist/", import.meta.url).pathname;

let entries;
try {
  entries = readdirSync(dist);
} catch {
  process.exit(0); // Nothing built yet.
}
for (const entry of entries) {
  if (entry === ".gitkeep") continue;
  rmSync(join(dist, entry), { recursive: true, force: true });
}
