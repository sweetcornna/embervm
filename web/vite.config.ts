import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The build lands inside pkg/webui so `go:embed` ships the console in the
// same binary as the API server — the self-hosting story is one artifact.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../pkg/webui/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // `npm run dev` against a locally running apiserver / embervm dev.
      // ws:true carries the /term WebSocket through the same proxy.
      "/v0": { target: "http://127.0.0.1:8080", ws: true },
    },
  },
});
