import { formatReleaseDate, precisionLabel, type DatePrecision } from "@/lib/date";

/**
 * Renders a release date at exactly the precision the vendor published,
 * per FirmScout's core promise (README "No invented dates"). The
 * supplementary precision explanation is available to everyone via a
 * native `title` tooltip and to screen reader users via visually-hidden
 * text, so the context survives without JavaScript.
 */
export function ReleaseDate({
  date,
  precision,
  className,
}: {
  date: string | undefined;
  precision: DatePrecision;
  className?: string;
}) {
  const formatted = formatReleaseDate(date, precision);
  const label = precisionLabel(precision);

  return (
    <span className={className} title={label}>
      {formatted}
      <span className="sr-only"> ({label})</span>
    </span>
  );
}
