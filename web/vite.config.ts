import { readdirSync, rmSync } from "node:fs";
import { resolve } from "node:path";
import react from "@vitejs/plugin-react";
import type { Plugin } from "vite";
import { defineConfig } from "vitest/config";

// The build is written to where the Go code embeds it from.
const outDir = resolve(import.meta.dirname, "../internal/web/dist");

// The Beacon that the development server passes the API on to.
const beacon = process.env.BEACON_URL ?? "http://127.0.0.1:18080";

// Empties the output directory before a build, except for the file that keeps
// it in git: without the directory the Go code does not build.
function cleanOutDir(): Plugin {
  return {
    name: "beacon:clean-out-dir",
    apply: "build",
    buildStart() {
      for (const entry of readdirSync(outDir)) {
        if (entry !== ".gitkeep") {
          rmSync(resolve(outDir, entry), { recursive: true, force: true });
        }
      }
    },
  };
}

export default defineConfig({
  plugins: [react(), cleanOutDir()],
  build: {
    outDir,
    emptyOutDir: false,
    // Everything is served by the binary: nothing is inlined into the page,
    // which the Content-Security-Policy would refuse.
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
  },
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    // The app and the API share one origin in development as in a release,
    // so the session cookie and the cross-site check behave the same.
    proxy: {
      "/api": { target: beacon },
      "/auth": { target: beacon },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    restoreMocks: true,
  },
});
