/**
 * Precision-aware date formatting.
 *
 * FirmScout's defining product rule: never display a date more precisely
 * than the vendor published it. The API returns `releaseDate` as
 * `YYYY-MM-DD`, `YYYY-MM`, `YYYY`, or omits it entirely, paired with a
 * `releaseDatePrecision` field that says which of those shapes to expect
 * (docs/architecture/api.md §4).
 *
 * This module MUST NOT run a partial date string through `Date.parse`,
 * `new Date(...)`, or any date library that would silently fill in a
 * missing day or month (e.g. `new Date("2026-09")` fills day=1 in the
 * local/UTC timezone). All formatting here is done with plain string
 * parsing so a month-precision date can never render a day.
 */

export type DatePrecision = "exact_day" | "month_only" | "year_only" | "unknown";

const MONTH_NAMES = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
] as const;

const NOT_PUBLISHED = "Release date not published";

/**
 * Render `date` according to `precision`. Never renders a component the
 * vendor did not publish.
 *
 * - `exact_day` + `YYYY-MM-DD` -> "3 September 2026"
 * - `month_only` + `YYYY-MM` -> "September 2026"
 * - `year_only` + `YYYY` -> "2026"
 * - `unknown` (date usually absent) -> "Release date not published"
 */
export function formatReleaseDate(
  date: string | undefined,
  precision: DatePrecision,
): string {
  if (precision === "unknown" || !date) {
    return NOT_PUBLISHED;
  }

  switch (precision) {
    case "exact_day": {
      const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
      if (!match) return NOT_PUBLISHED;
      const [, year, month, day] = match as unknown as [string, string, string, string];
      const monthIndex = Number(month) - 1;
      const monthName = MONTH_NAMES[monthIndex];
      if (!monthName) return NOT_PUBLISHED;
      return `${Number(day)} ${monthName} ${year}`;
    }
    case "month_only": {
      const match = /^(\d{4})-(\d{2})$/.exec(date);
      if (!match) return NOT_PUBLISHED;
      const [, year, month] = match as unknown as [string, string, string];
      const monthIndex = Number(month) - 1;
      const monthName = MONTH_NAMES[monthIndex];
      if (!monthName) return NOT_PUBLISHED;
      return `${monthName} ${year}`;
    }
    case "year_only": {
      const match = /^(\d{4})$/.exec(date);
      if (!match) return NOT_PUBLISHED;
      return date;
    }
    default:
      return NOT_PUBLISHED;
  }
}

/**
 * A short, human-readable explanation of what a given precision means,
 * suitable for a tooltip next to a rendered date.
 */
export function precisionLabel(precision: DatePrecision): string {
  switch (precision) {
    case "exact_day":
      return "The vendor published the exact release day.";
    case "month_only":
      return "The vendor published only the month.";
    case "year_only":
      return "The vendor published only the year.";
    case "unknown":
      return "The vendor has not published a release date for this version.";
    default:
      return "Release date precision is unknown.";
  }
}
