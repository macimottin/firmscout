/** See official-sources-list.test.ts for why this is React.createElement + renderToStaticMarkup. */
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ConflictBanner } from "./conflict-banner";
import type { SourceConflictDetail } from "@/lib/api";

const detail: SourceConflictDetail = {
  channel: "stable",
  versions: ["7.15.1", "7.15.2"],
  sourceCount: 2,
  detectedAt: "2026-08-30T12:00:00Z",
};

describe("ConflictBanner", () => {
  it("renders the real channel and disputed versions when detail is present", () => {
    const html = renderToStaticMarkup(createElement(ConflictBanner, { detail }));
    expect(html).toContain("stable");
    expect(html).toContain("7.15.1");
    expect(html).toContain("7.15.2");
  });

  it("falls back to today's generic banner text when detail is null (unrefreshed summary)", () => {
    const html = renderToStaticMarkup(createElement(ConflictBanner, { detail: null }));
    expect(html).toContain("Two or more official sources disagree");
    expect(html).not.toContain("undefined");
  });

  it("falls back to today's generic banner text when detail is entirely absent", () => {
    const html = renderToStaticMarkup(createElement(ConflictBanner, {}));
    expect(html).toContain("Two or more official sources disagree");
  });
});
