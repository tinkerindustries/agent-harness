import { writeFileSync } from "node:fs";
import { fileURLToPath, URL } from "node:url";
import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// emptyOutDir wipes the directory on every build, .gitkeep included. That
// file is committed — go:embed will not compile against a missing directory,
// so a clone without it cannot build the binary at all — which left every
// build reporting a deletion that a `git add -A` would then stage. Putting it
// back as part of the build keeps the working tree clean and the trap shut.
function keepGitkeep(outDir: string) {
  return {
    name: "keep-gitkeep",
    closeBundle() {
      writeFileSync(new URL(`${outDir}/.gitkeep`, import.meta.url), "");
    },
  };
}

// Build output lands in internal/webassets/dist, where
// internal/webassets/embed.go embeds it into the harness binary
// (docs/DESIGN.md §4.8: "One binary, no runtime assets"). Dev mode
// (`npm run dev`) is unaffected by outDir; `harness serve -dev-frontend
// http://127.0.0.1:5173` proxies to this dev server instead of reading
// the built assets. The tailwindcss() plugin is the Tailwind v4 integration.
export default defineConfig(({ mode }) => {
  // The dev server's own port and its /api proxy target come from the repo
  // root .env — not web/.env — because that's the file docker compose and
  // `wt init` both read and write (docs/wt.md). Passing
  // "" as loadEnv's prefix lifts every var, not just VITE_-prefixed ones;
  // outside a worktree neither var is set and both defaults below match
  // today's behaviour exactly.
  const rootEnv = loadEnv(mode, fileURLToPath(new URL("..", import.meta.url)), "");
  const harnessHTTPPort = rootEnv.HARNESS_HTTP_PORT || "8080";
  const vitePort = Number(rootEnv.HARNESS_VITE_PORT) || 5173;

  return {
    plugins: [react(), tailwindcss(), keepGitkeep("../internal/webassets/dist")],
    resolve: {
      alias: {
        "@": fileURLToPath(new URL("./src", import.meta.url)),
      },
    },
    build: {
      outDir: "../internal/webassets/dist",
      emptyOutDir: true,
    },
    server: {
      port: vitePort,
      // Only pinned once a worktree has allocated this port on purpose —
      // the main checkout keeps today's silent-auto-bump behaviour.
      strictPort: rootEnv.HARNESS_VITE_PORT !== undefined,
      proxy: {
        "/api": `http://127.0.0.1:${harnessHTTPPort}`,
      },
    },
  };
});
