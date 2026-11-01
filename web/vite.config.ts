import { defineConfig } from "vitest/config";
import solid from "vite-plugin-solid";
import tailwind from "@tailwindcss/vite";

// The committed browser bundle is embedded by the Rust API.
export default defineConfig({
  plugins: [solid(), tailwind()],
  build: { outDir: "../api/assets", emptyOutDir: true, sourcemap: false, target: "es2022" },
  test: { environment: "node", include: ["src/core/**/*.test.ts"] }, // the core is framework-free: no DOM
  server: {
    proxy: {
      "/api": { target: "http://127.0.0.1:4999", changeOrigin: true, headers: { Origin: "http://127.0.0.1:4999" } },
      "/ws": { target: "ws://127.0.0.1:4999", ws: true, changeOrigin: true, headers: { Origin: "http://127.0.0.1:4999" } },
    },
  },
});
