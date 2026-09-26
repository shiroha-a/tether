import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const backend = process.env.TETHER_BACKEND ?? "http://127.0.0.1:3100";

export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true, chunkSizeWarningLimit: 1000 },
  server: {
    proxy: {
      "/api": backend,
      "/ws": { target: backend, ws: true },
    },
  },
});
