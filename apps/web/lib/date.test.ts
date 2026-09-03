import { describe, expect, it } from "vitest";
import { formatReleaseDate, precisionLabel } from "./date";

describe("formatReleaseDate", () => {
  it("renders exact_day as a full date with no invented components", () => {
    expect(formatReleaseDate("2026-09-02", "exact_day")).toBe("2 September 2026");
    expect(formatReleaseDate("2026-01-01", "exact_day")).toBe("1 January 2026");
  });

  it("renders month_only as month + year and NEVER includes a day", () => {
    const result = formatReleaseDate("2026-02", "month_only");
    expect(result).toBe("February 2026");
    // The defining product rule: a month-precision date must never render
    // a day-of-month, whether numeric or invented.
    expect(result).not.toMatch(/\b\d{1,2}\s+February\b/);
    expect(result).not.toContain("01 February");
    expect(result).not.toContain("1 February");
  });

  it("renders year_only as the bare year with no month or day", () => {
    const result = formatReleaseDate("2026", "year_only");
    expect(result).toBe("2026");
    expect(result).not.toMatch(/January|February|March/);
  });

  it("renders unknown precision as an explicit not-published string, never a fabricated date", () => {
    expect(formatReleaseDate(undefined, "unknown")).toBe("Release date not published");
    // Even if a date string were somehow present alongside "unknown"
    // precision, the explicit not-published string must win.
    expect(formatReleaseDate("2026-09-02", "unknown")).toBe("Release date not published");
  });

  it("falls back to the not-published string when the date is missing regardless of precision", () => {
    expect(formatReleaseDate(undefined, "exact_day")).toBe("Release date not published");
    expect(formatReleaseDate(undefined, "month_only")).toBe("Release date not published");
    expect(formatReleaseDate(undefined, "year_only")).toBe("Release date not published");
  });

  it("falls back to the not-published string when the date does not match the claimed precision", () => {
    // A month-precision field carrying a full YYYY-MM-DD string would be a
    // contract violation from the API; fail safe rather than guess.
    expect(formatReleaseDate("2026-09-02", "month_only")).toBe("Release date not published");
    expect(formatReleaseDate("2026-09", "year_only")).toBe("Release date not published");
  });
});

describe("precisionLabel", () => {
  it("returns a distinct, explanatory phrase for each precision", () => {
    const labels = [
      precisionLabel("exact_day"),
      precisionLabel("month_only"),
      precisionLabel("year_only"),
      precisionLabel("unknown"),
    ];
    expect(new Set(labels).size).toBe(4);
    for (const label of labels) {
      expect(label.length).toBeGreaterThan(0);
    }
  });

  it("describes month_only precision explicitly", () => {
    expect(precisionLabel("month_only")).toMatch(/month/i);
  });

  it("describes unknown precision as not published", () => {
    expect(precisionLabel("unknown")).toMatch(/not published/i);
  });
});
