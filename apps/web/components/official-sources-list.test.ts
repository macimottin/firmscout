/**
 * Uses React.createElement + renderToStaticMarkup instead of JSX (kept as a plain
 * .test.ts, matching this repo's vitest include glob and its no-jsdom convention) to
 * prove the extracted list actually renders real `<a href>` links, not just that its
 * prop-shaping logic is correct.
 */
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { OfficialSourcesList } from "./official-sources-list";
import type { OfficialSource } from "@/lib/api";

const sources: OfficialSource[] = [
  { url: "https://mikrotik.com/download", kind: "html", official: true },
  { url: "https://forum.mikrotik.com/changelog", kind: "html", official: false },
];

describe("OfficialSourcesList", () => {
  it("renders a real <a href> link per source, with an Official badge only on the official one", () => {
    const html = renderToStaticMarkup(createElement(OfficialSourcesList, { sources }));
    expect(html).toContain('href="https://mikrotik.com/download"');
    expect(html).toContain('href="https://forum.mikrotik.com/changelog"');
    expect(html).toContain("Official");
    // Exactly one badge: the unofficial source's <li> must not also claim "Official".
    expect(html.match(/Official/g)?.length).toBe(1);
  });

  it("renders the honest empty state rather than fabricate a source for a product with none", () => {
    const html = renderToStaticMarkup(createElement(OfficialSourcesList, { sources: [] }));
    expect(html).toContain("No official sources recorded.");
    expect(html).not.toContain("<a ");
  });
});
