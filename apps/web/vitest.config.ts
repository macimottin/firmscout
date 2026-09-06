import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

export default defineConfig({
  resolve: {
    // Mirrors tsconfig.json's `paths["@/*"]` -- vitest does not read tsconfig path
    // mapping on its own, so a component test that imports "@/components/ui/badge"
    // (as any component under components/ legitimately does) needs this to resolve
    // at all, not just to type-check.
    alias: {
      "@": fileURLToPath(new URL(".", import.meta.url)),
    },
  },
  test: {
    environment: "node",
    include: ["**/*.test.ts"],
  },
});
