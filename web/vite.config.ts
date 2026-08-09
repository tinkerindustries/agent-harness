import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Build output lands in internal/webassets/dist, where
// internal/webassets/embed.go embeds it into the harness binary
// (docs/DESIGN.md §4.8: "One binary, no runtime assets"). Dev mode
// (`npm run dev`) is unaffected by outDir; `harness serve -dev-frontend
// http://127.0.0.1:5173` proxies to this dev server instead of reading
// the built assets.
export default defineConfig({
  plugins: [react()],
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
