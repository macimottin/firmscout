import { describe, expect, it } from "vitest";
import {
  evidenceExcerpt,
  formatUtcDay,
  formatUtcMinute,
  parsedEvidenceRetrievedAt,
  sourceLabel,
  sourceUrl,
} from "./release-display";
import type { Evidence, ReleaseSource } from "./api";

const officialSource: ReleaseSource = {
  official: true,
  url: "https://mikrotik.com/download",
  kind: "html",
};

const evidence: Evidence = {
  retrievedAt: "2026-09-01T08:00:00Z",
  excerpt: "RouterOS 7.15.2 stable release notes",
};

describe("sourceUrl / sourceLabel", () => {
  it("returns the real url and label for a present, official source", () => {
    expect(sourceUrl(officialSource)).toBe("https://mikrotik.com/download");
    expect(sourceLabel(officialSource)).toBe("official source");
  });

  it("labels a non-official source distinctly", () => {
    expect(sourceLabel({ ...officialSource, official: false })).toBe("source");
  });

  // Simulates a stale cache or pre-fix API response: the type says `source` is
  // required, but a real malformed value must not crash the accessor.
  it("does not throw and returns a safe fallback when source is missing", () => {
    const missing = undefined as unknown as ReleaseSource;
    expect(() => sourceUrl(missing)).not.toThrow();
    expect(() => sourceLabel(missing)).not.toThrow();
    expect(sourceUrl(missing)).toBeNull();
    expect(sourceLabel(missing)).toBe("source");
  });

  it("does not throw when source is explicitly null", () => {
    expect(sourceUrl(null)).toBeNull();
    expect(sourceLabel(null)).toBe("source");
  });
});

describe("evidenceExcerpt / parsedEvidenceRetrievedAt", () => {
  it("returns the real excerpt and a valid parsed date when evidence is present", () => {
    expect(evidenceExcerpt(evidence)).toBe(
      "RouterOS 7.15.2 stable release notes",
    );
    expect(parsedEvidenceRetrievedAt(evidence)?.toISOString()).toBe(
      "2026-09-01T08:00:00.000Z",
    );
  });

  // Same production-crash class as above, this time for evidence: a release fixture
  // without an `evidence` field must render a fallback, not throw.
  it("does not throw and returns null when evidence is missing", () => {
    const missing = undefined as unknown as Evidence;
    expect(() => evidenceExcerpt(missing)).not.toThrow();
    expect(() => parsedEvidenceRetrievedAt(missing)).not.toThrow();
    expect(evidenceExcerpt(missing)).toBeNull();
    expect(parsedEvidenceRetrievedAt(missing)).toBeNull();
  });

  it("returns null for an unparseable retrievedAt rather than an Invalid Date", () => {
    const malformed: Evidence = { ...evidence, retrievedAt: "not-a-timestamp" };
    const parsed = parsedEvidenceRetrievedAt(malformed);
    expect(parsed).toBeNull();
  });

  it("returns null for an empty excerpt rather than an empty string standing in for missing", () => {
    expect(evidenceExcerpt({ ...evidence, excerpt: "" })).toBeNull();
  });
});

describe("formatUtcMinute / formatUtcDay", () => {
  // The regression these exist for: `lastVerifiedAt` is omitempty on the wire, a
  // hardware model has no verifying source, and `new Date(undefined).toISOString()`
  // throws RangeError rather than returning a placeholder. In a server component that
  // is a 500 for the entire page -- which is what the product page and the search
  // page returned for the first six devices added to the catalogue.
  it("returns null instead of throwing when the timestamp is absent", () => {
    const absent = undefined as unknown as string;
    expect(() => formatUtcMinute(absent)).not.toThrow();
    expect(() => formatUtcDay(absent)).not.toThrow();
    expect(formatUtcMinute(absent)).toBeNull();
    expect(formatUtcDay(absent)).toBeNull();
    expect(formatUtcMinute(null)).toBeNull();
    expect(formatUtcDay(null)).toBeNull();
  });

  it("returns null for an unparseable timestamp rather than 'Invalid Date'", () => {
    expect(formatUtcMinute("not-a-timestamp")).toBeNull();
    expect(formatUtcDay("not-a-timestamp")).toBeNull();
  });

  it("renders a real instant in UTC at each granularity", () => {
    expect(formatUtcMinute("2026-09-05T22:02:17Z")).toBe("2026-09-05 22:02");
    expect(formatUtcDay("2026-09-05T22:02:17Z")).toBe("2026-09-05");
  });

  // An offset timestamp must be normalised to UTC, not echoed back, or the "UTC"
  // suffix the page prints beside it would be a false label.
  it("normalises a non-UTC offset to UTC", () => {
    expect(formatUtcMinute("2026-09-05T19:02:17-03:00")).toBe(
      "2026-09-05 22:02",
    );
  });
});
