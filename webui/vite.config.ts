import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";

// `npm run dev` serves the app on :5173 against a running infermux-ui.
export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  server: {
    proxy: {
      "/api": "http://127.0.0.1:5010",
      "/daemon": "http://127.0.0.1:5010",
    },
  },
});
