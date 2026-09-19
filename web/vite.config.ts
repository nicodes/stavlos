import { defineConfig } from "vitest/config";
import solid from "vite-plugin-solid";
import tailwind from "@tailwindcss/vite";

// The bundle is committed under internal/web/dist and go:embedded, so
// `go install` needs no Node (docs/web-ui.md). `npm run dev` proxies the API
// and the socket to a daemon with the web UI on.
export default defineConfig({
  plugins: [solid(), tailwind()],
  build: { outDir: "../internal/web/dist", emptyOutDir: true, sourcemap: false, target: "es2022" },
  test: { environment: "node", include: ["src/core/**/*.test.ts"] }, // the core is framework-free: no DOM
  server: {
    proxy: {
      "/api": { target: "http://127.0.0.1:4999", changeOrigin: true, headers: { Origin: "http://127.0.0.1:4999" } },
      "/ws": { target: "ws://127.0.0.1:4999", ws: true, changeOrigin: true, headers: { Origin: "http://127.0.0.1:4999" } },
    },
  },
});
