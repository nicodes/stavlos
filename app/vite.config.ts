import { defineConfig } from "vitest/config";
import solid from "vite-plugin-solid";
import tailwind from "@tailwindcss/vite";

// Ignored generated staging is embedded by the Rust API when packaging.
export default defineConfig({ envDir: false,
  plugins: [solid(), tailwind()],
  build: { outDir: "../.artifacts/ui", emptyOutDir: true, sourcemap: false, target: "es2022" },
  test: { environment: "node", include: ["src/core/**/*.test.ts"] }, // the core is framework-free: no DOM
  server: {
    proxy: {
      "/api": { target: "http://127.0.0.1:4999", changeOrigin: true, headers: { Origin: "http://127.0.0.1:4999" } },
      "/ws": { target: "ws://127.0.0.1:4999", ws: true, changeOrigin: true, headers: { Origin: "http://127.0.0.1:4999" } },
    },
  },
});
