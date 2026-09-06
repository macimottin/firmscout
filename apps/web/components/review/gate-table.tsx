import { Badge } from "@/components/ui/badge";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { GateResult } from "@/lib/review";

const outcomeVariant: Record<string, "success" | "warning" | "danger" | "default"> = {
  passed: "success",
  review_required: "warning",
  rejected: "danger",
};

/**
 * The recorded verdict of every gate that ran for this candidate, in gate
 * order — without this a reviewer can see that a candidate needs review
 * but not which check said so, which is exactly the gap
 * CandidateRepository.ListValidationResults exists to close. The row
 * whose outcome is not "passed" is the one that routed this item here;
 * it is highlighted rather than singled out by a separate label, since a
 * candidate can only be routed to review or rejected by one such gate,
 * but the whole ladder is worth showing.
 */
export function GateTable({ gates }: { gates: GateResult[] }) {
  if (gates.length === 0) {
    return <p className="text-sm text-ink-500">No gate verdicts were recorded for this candidate.</p>;
  }

  return (
    <Table>
      <caption className="sr-only">Validation gate results, in gate order</caption>
      <TableHeader>
        <TableRow>
          <TableHead>Order</TableHead>
          <TableHead>Gate</TableHead>
          <TableHead>Outcome</TableHead>
          <TableHead>Detail</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {gates.map((gate) => (
          <TableRow
            key={gate.order}
            className={gate.outcome !== "passed" ? "bg-amber-500/5" : undefined}
          >
            <TableCell className="text-ink-500">{gate.order}</TableCell>
            <TableCell className="text-ink-900">{gate.gate.replace(/_/g, " ")}</TableCell>
            <TableCell>
              <Badge variant={outcomeVariant[gate.outcome] ?? "default"}>
                {gate.outcome.replace(/_/g, " ")}
              </Badge>
            </TableCell>
            <TableCell className="text-ink-600">{gate.detail ?? "—"}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
