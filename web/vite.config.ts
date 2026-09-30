/// <reference types="vitest/config" />
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Two personalities, one config (ADR 0030) — the same arrangement as booth-catalog/booth-storage:
//   - `vite`/`vite dev`: the dev harness (index.html → src/main.tsx → src/devshell/DevShell.tsx),
//     a stand-in for booth-design's shell.
//   - `vite build`: the publishable library (@projectbooth/database-ui) from src/index.ts, with
//     react/react-dom external so booth-design's own copies are used.
export default defineConfig(({ command }) => ({
  plugins: [react()],
  build:
    command === "build"
      ? {
          lib: {
            entry: fileURLToPath(new URL("./src/index.ts", import.meta.url)),
            formats: ["es"],
            fileName: "index",
          },
          rollupOptions: {
            external: ["react", "react-dom", "react/jsx-runtime"],
          },
        }
      : undefined,
  server: {
    proxy: {
      // Local dev only: booth-core's gateway normally proxies /modules/database/* to this module's
      // own /api/* routes, validating the browser's X-Workspace and forwarding it as
      // X-Booth-Workspace (ADR 0025). The proxy mimics both, so the harness works against a
      // locally running backend (BOOTH_DATABASE_DEV_BACKEND).
      "/modules/database": {
        target: process.env.BOOTH_DATABASE_DEV_BACKEND ?? "http://localhost:8080",
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/modules\/database/, ""),
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyReq, req) => {
            const ws = req.headers["x-workspace"];
            if (typeof ws === "string" && ws !== "") proxyReq.setHeader("X-Booth-Workspace", ws);
          });
        },
      },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/setupTests.ts"],
  },
}));
