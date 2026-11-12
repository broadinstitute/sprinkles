import { defineConfig } from "vite";

// Ladle bundles its own (older) Vite and React plugin, so it can't load the
// app's vite.config.ts: that one uses @vitejs/plugin-react v6, which targets
// Vite 8 / Rolldown and fails under Ladle's Vite with "Missing field
// `moduleType`". Ladle adds its own React plugin, so none is needed here.
export default defineConfig({});
