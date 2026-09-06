import { describe, expect, it } from "vitest";
import { describeConflict } from "./conflict";
import type { SourceConflictDetail } from "./api";

const detail: SourceConflictDetail = {
  channel: "stable",
  versions: ["7.15.1", "7.15.2"],
  sourceCount: 2,
  detectedAt: "2026-08-30T12:00:00Z",
};

describe("describeConflict", () => {
  it("renders the real channel, versions, and source count when detail is present", () => {
    const banner = describeConflict(detail);
    expect(banner.title).toContain("stable");
    expect(banner.description).toContain("stable");
    expect(banner.description).toContain("7.15.1");
    expect(banner.description).toContain("7.15.2");
    expect(banner.description).toContain("2 eligible sources");
    expect(banner.description).toContain("2026-08-30");
  });

  it("uses singular 'source' for a single-source conflict", () => {
    const banner = describeConflict({ ...detail, sourceCount: 1 });
    expect(banner.description).toContain("1 eligible source report");
    expect(banner.description).not.toContain("1 eligible sources");
  });

  it("falls back to the generic banner when detail is null (unrefreshed or predates this field)", () => {
    const banner = describeConflict(null);
    expect(banner.title).toBe("Conflicting sources");
    expect(banner.description).toContain("Two or more official sources disagree");
    expect(banner.description).not.toContain("undefined");
  });

  it("falls back to the generic banner when detail is undefined", () => {
    const banner = describeConflict(undefined);
    expect(banner.title).toBe("Conflicting sources");
    expect(banner.description).toContain("Two or more official sources disagree");
  });

  it("falls back to the generic banner rather than render an empty version list", () => {
    const banner = describeConflict({ ...detail, versions: [] });
    expect(banner.title).toBe("Conflicting sources");
    expect(banner.description).not.toContain("reported versions are");
  });

  it("falls back to the generic banner rather than claim a non-positive source count", () => {
    const banner = describeConflict({ ...detail, sourceCount: 0 });
    expect(banner.title).toBe("Conflicting sources");
  });

  it("omits the detected-at parenthetical rather than show an invalid date", () => {
    const banner = describeConflict({ ...detail, detectedAt: "not-a-date" });
    expect(banner.description).not.toContain("detected");
    expect(banner.description).not.toContain("Invalid Date");
  });

  it("renders real versions and count without a dangling 'on the  channel' when the disputing sources stated no channel", () => {
    const banner = describeConflict({ ...detail, channel: "" });
    expect(banner.title).toBe("Conflicting sources");
    expect(banner.description).not.toContain("channel");
    expect(banner.description).not.toContain("  ");
    expect(banner.description).toContain("7.15.1");
    expect(banner.description).toContain("7.15.2");
    expect(banner.description).toContain("2 eligible sources");
  });
});
