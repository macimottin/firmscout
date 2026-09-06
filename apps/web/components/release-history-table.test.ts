/**
 * See official-sources-list.test.ts for why this is React.createElement +
 * renderToStaticMarkup in a plain .test.ts rather than JSX.
 */
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ReleaseHistoryTable } from "./release-history-table";
import type { Release } from "@/lib/api";

function release(overrides: Partial<Release> = {}): Release {
  return {
    id: "rel_arm_7242",
    product: { slug: "mikrotik-routeros", name: "RouterOS" },
    rawVersion: "7.24.2",
    normalizedVersion: "7.24.2",
    releaseType: "embedded_os",
    channel: "stable",
    releaseDate: "2026-02",
    releaseDatePrecision: "month_only",
    recommended: null,
    withdrawn: false,
    correctsReleaseId: null,
    source: { official: true, url: "https://mikrotik.com/download/changelogs", kind: "html" },
    evidence: { retrievedAt: "2026-02-10T00:00:00Z", excerpt: "RouterOS 7.24.2 stable." },
    firstObservedAt: "2026-02-10T00:00:00Z",
    lastVerifiedAt: "2026-02-10T00:00:00Z",
    ...overrides,
  };
}

describe("ReleaseHistoryTable", () => {
  it("renders the releases that apply to a device model", () => {
    const html = renderToStaticMarkup(
      createElement(ReleaseHistoryTable, {
        releases: [release()],
        productName: "hAP be lite",
      }),
    );
    expect(html).toContain("7.24.2");
    expect(html).toContain("stable");
    expect(html).toContain("February 2026");
  });

  it("never renders a full day for a release whose date the vendor published only to the month", () => {
    const html = renderToStaticMarkup(
      createElement(ReleaseHistoryTable, {
        releases: [release({ releaseDate: "2026-02", releaseDatePrecision: "month_only" })],
        productName: "hAP be lite",
      }),
    );
    expect(html).toContain("February 2026");
    expect(html).not.toMatch(/\b\d{1,2}\s+February\b/);
    expect(html).not.toContain("1 February");
  });

  it("renders the honest empty body for a device with no applicable releases recorded, rather than crash", () => {
    expect(() =>
      renderToStaticMarkup(createElement(ReleaseHistoryTable, { releases: [], productName: "RB433" })),
    ).not.toThrow();
  });
});
