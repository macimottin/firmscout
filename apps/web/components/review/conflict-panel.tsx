import { ReleaseDate } from "@/components/release-date";
import { Badge } from "@/components/ui/badge";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Conflict } from "@/lib/review";

/**
 * The competing versions behind a multi-source disagreement, and what
 * each source currently claims — loaded live rather than from the
 * conflict row (application.ReviewItemDetail's doc comment: "a reviewer
 * deciding today needs today's claims"). FirmScout never auto-resolves a
 * same-tier or two-official disagreement (ADR-0020); this panel is the
 * whole of what a human is given to decide it with.
 */
export function ConflictPanel({ conflict }: { conflict: Conflict }) {
  return (
    <div className="rounded-lg border border-amber-500/40 bg-amber-500/5 p-5">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="warning">Unresolved conflict</Badge>
        <span className="text-sm text-ink-600">
          authority rank {conflict.authorityRank} · {conflict.observations.length} source(s) reporting
        </span>
      </div>
      <p className="mt-3 text-sm text-ink-700">
        Versions in dispute:{" "}
        <span className="font-mono">{conflict.versions.join(", ")}</span>
      </p>

      <div className="mt-4">
        <Table>
          <caption className="sr-only">What each source currently reports</caption>
          <TableHeader>
            <TableRow>
              <TableHead>Source</TableHead>
              <TableHead>Quality class</TableHead>
              <TableHead>Reports</TableHead>
              <TableHead>Release date</TableHead>
              <TableHead>Eligible</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {conflict.observations.map((obs) => (
              <TableRow key={obs.sourceId}>
                <TableCell className="text-ink-900">
                  {obs.sourceId}
                  {obs.official ? (
                    <Badge variant="success" className="ml-2">
                      Official
                    </Badge>
                  ) : null}
                </TableCell>
                <TableCell className="text-ink-600">{obs.qualityClass.replace(/_/g, " ")}</TableCell>
                <TableCell className="font-mono text-ink-900">{obs.rawVersion}</TableCell>
                <TableCell className="text-ink-600">
                  <ReleaseDate date={obs.releaseDate} precision={obs.releaseDatePrecision} />
                </TableCell>
                <TableCell className="text-ink-600">{obs.eligible ? "Yes" : "No"}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}
