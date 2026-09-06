import Link from "next/link";
import { ReleaseDate } from "@/components/release-date";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { Release } from "@/lib/api";

/**
 * The product page's "Version history" table -- the releases that apply to this
 * product, which for a device Product (ADR-0024) is the empty case today (zero
 * `release_product_mappings` rows) and, for a product with its own release
 * stream, the real history. Extracted verbatim out of the page component, with no
 * behaviour change, so it can be exercised directly against a fixture
 * (release-history-table.test.ts) -- in particular a release whose date the
 * vendor published only to the month, proving the precision rule holds here too,
 * without needing a running API.
 */
export function ReleaseHistoryTable({
  releases,
  productName,
}: {
  releases: Release[];
  productName: string;
}) {
  return (
    <Table>
      <caption className="sr-only">Version history for {productName}</caption>
      <TableHeader>
        <TableRow>
          <TableHead>Version</TableHead>
          <TableHead>Channel</TableHead>
          <TableHead>Release date</TableHead>
          <TableHead>Recommended</TableHead>
          <TableHead>Status</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {releases.map((release) => (
          <TableRow key={release.id}>
            <TableCell>
              <Link
                href={`/releases/${release.id}`}
                className="font-mono text-ink-900 underline-offset-2 hover:underline"
              >
                {release.rawVersion}
              </Link>
            </TableCell>
            <TableCell className="text-ink-600">{release.channel}</TableCell>
            <TableCell className="text-ink-600">
              <ReleaseDate date={release.releaseDate} precision={release.releaseDatePrecision} />
            </TableCell>
            <TableCell className="text-ink-600">
              {release.recommended === true ? "Yes" : "—"}
            </TableCell>
            <TableCell className="text-ink-600">
              {release.withdrawn ? <Badge variant="danger">Withdrawn</Badge> : "Published"}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
