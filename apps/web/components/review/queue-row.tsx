import Link from "next/link";
import { Badge } from "@/components/ui/badge";
import { TableCell, TableRow } from "@/components/ui/table";
import type { ReviewItem, SlaClass } from "@/lib/review";

const slaVariant: Record<SlaClass, "danger" | "warning" | "default" | "outline"> = {
  urgent: "danger",
  high: "warning",
  standard: "default",
  low: "outline",
};

const kindLabel: Record<string, string> = {
  multi_source_conflict: "Multi-source conflict",
  gate_failed: "Failed a validation gate",
  low_confidence: "Low confidence",
  new_vendor: "New vendor",
  new_product: "New product",
};

function formatKind(kind: string): string {
  return kindLabel[kind] ?? kind.replace(/_/g, " ");
}

/**
 * How long an item has waited, in the coarsest unit a human scans a queue
 * with. This is presentation only — it is never the priority score itself
 * (domain.ScoreReview deliberately excludes age; the queue index already
 * orders by age within a priority band, see ports.go's ReviewQueueFilter
 * doc comment) — just what a reviewer's eye looks for first.
 */
function formatAge(ageSeconds: number): string {
  if (ageSeconds < 60) return "just now";
  const minutes = Math.floor(ageSeconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  return `${days}d`;
}

export function QueueRow({ item }: { item: ReviewItem }) {
  return (
    <TableRow>
      <TableCell>
        <Link
          href={`/review/${encodeURIComponent(item.id)}`}
          className="font-medium text-ink-900 underline-offset-2 hover:underline"
        >
          {item.title}
        </Link>
        {item.detail ? <p className="mt-0.5 text-xs text-ink-500">{item.detail}</p> : null}
      </TableCell>
      <TableCell className="text-ink-600">{formatKind(item.kind)}</TableCell>
      <TableCell>
        <Badge variant={slaVariant[item.slaClass]}>{item.slaClass}</Badge>
      </TableCell>
      <TableCell className="text-ink-600">{item.priorityScore}</TableCell>
      <TableCell className="text-ink-600">
        {item.vendor ? (
          <Link href={`/vendors/${item.vendor.slug}`} className="hover:underline">
            {item.vendor.name}
          </Link>
        ) : (
          "—"
        )}
      </TableCell>
      <TableCell className="text-ink-600">
        {item.product?.slug ? (
          <Link href={`/products/${item.product.slug}`} className="hover:underline">
            {item.product.name}
          </Link>
        ) : (
          item.product?.name ?? "—"
        )}
      </TableCell>
      <TableCell className="text-ink-600">{formatAge(item.ageSeconds)}</TableCell>
    </TableRow>
  );
}
