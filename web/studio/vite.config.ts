import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies the API to a running `docsync studio`. Start the studio with a
// fixed token and pass the same token here:
//   docsync studio --token dev --no-browser
//   DOCSYNC_STUDIO_TOKEN=dev npm run dev
const target = process.env.DOCSYNC_STUDIO_URL ?? "http://127.0.0.1:8090";
const token = process.env.DOCSYNC_STUDIO_TOKEN ?? "";
const proxy = {
  target,
  changeOrigin: true,
  headers: token ? { Authorization: `Bearer ${token}` } : undefined,
};

export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    // The Go binary embeds this directory (internal/studio/server.go).
    outDir: "../../internal/studio/ui/dist",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
  server: { proxy: { "/api": proxy, "/pipeline": proxy } },
});
