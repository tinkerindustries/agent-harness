import { writeFileSync } from "node:fs";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

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
// the built assets.
export default defineConfig({
  plugins: [react(), keepGitkeep("../internal/webassets/dist")],
  build: {
    outDir: "../internal/webassets/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8080",
    },
  },
});
