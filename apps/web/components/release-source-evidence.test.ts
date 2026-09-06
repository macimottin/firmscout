/**
 * See official-sources-list.test.ts for why this is React.createElement +
 * renderToStaticMarkup in a plain .test.ts rather than JSX.
 *
 * This is the direct regression test for the bug this task exists to fix: the
 * release detail page 500ing because `source`/`evidence` were never populated. The
 * backend guarantee is real now, but this proves the frontend's OWN defense holds
 * even if a malformed or stale-cached response reaches it anyway.
 */
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ReleaseSourceEvidence } from "./release-source-evidence";
import type { Evidence, ReleaseSource } from "@/lib/api";

const source: ReleaseSource = {
  official: true,
  url: "https://mikrotik.com/download/changelogs",
  kind: "html",
};

const evidence: Evidence = {
  retrievedAt: "2026-09-01T08:00:00Z",
  excerpt: "RouterOS 7.15.2 has been released.",
};

describe("ReleaseSourceEvidence", () => {
  it("renders the real source link, Official badge, and evidence excerpt when both are present", () => {
    const html = renderToStaticMarkup(createElement(ReleaseSourceEvidence, { source, evidence }));
    expect(html).toContain('href="https://mikrotik.com/download/changelogs"');
    expect(html).toContain("Official");
    expect(html).toContain("RouterOS 7.15.2 has been released.");
    expect(html).toContain("2026-09-01");
  });

  // The exact fixture shape this task exists to guard against: a stale cache or an
  // old API version handing this component a release with no `source`.
  it("does not throw and renders an honest fallback when source is missing", () => {
    const staleRelease = { evidence } as unknown as { source: ReleaseSource; evidence: Evidence };
    expect(() =>
      renderToStaticMarkup(
        createElement(ReleaseSourceEvidence, { source: staleRelease.source, evidence: staleRelease.evidence }),
      ),
    ).not.toThrow();

    const html = renderToStaticMarkup(
      createElement(ReleaseSourceEvidence, { source: staleRelease.source, evidence: staleRelease.evidence }),
    );
    expect(html).toContain("Source not recorded");
    expect(html).not.toContain("undefined");
  });

  it("does not throw and renders an honest fallback when evidence is missing", () => {
    const staleRelease = { source } as unknown as { source: ReleaseSource; evidence: Evidence };
    const html = renderToStaticMarkup(
      createElement(ReleaseSourceEvidence, { source: staleRelease.source, evidence: staleRelease.evidence }),
    );
    expect(html).toContain("Retrieval time not recorded");
    expect(html).toContain("No evidence excerpt recorded.");
    expect(html).not.toContain("undefined");
  });

  it("does not throw when both source and evidence are missing", () => {
    expect(() =>
      renderToStaticMarkup(createElement(ReleaseSourceEvidence, { source: undefined, evidence: undefined })),
    ).not.toThrow();
  });
});
