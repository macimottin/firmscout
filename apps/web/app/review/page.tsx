import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { isUnreachable } from "@/lib/api";
import { listReviewItems, reviewUiEnabled, type ReviewItem, type SlaClass } from "@/lib/review";
import { Unavailable } from "@/components/unavailable";
import { UnauthenticatedBanner } from "@/components/review/unauthenticated-banner";
import { QueueRow } from "@/components/review/queue-row";
import { Badge } from "@/components/ui/badge";
import { Table, TableBody, TableHead, TableHeader, TableRow } from "@/components/ui/table";

export const metadata: Metadata = {
  title: "Review queue",
  // Never indexed: this is an internal, unauthenticated operational
  // surface (ADR-0021), not a page for the public catalogue's audience.
  robots: { index: false, follow: false },
};

// The queue changes under a reviewer's feet with every accept/reject; the
// backing API already sends Cache-Control: no-store for this reason, and
// this page must not paper over that with static rendering.
export const revalidate = 0;

const SLA_CLASSES: SlaClass[] = ["urgent", "high", "standard", "low"];

interface ReviewQueuePageProps {
  searchParams: Promise<{ sla?: string }>;
}

export default async function ReviewQueuePage({ searchParams }: ReviewQueuePageProps) {
  if (!reviewUiEnabled()) {
    // This build has not switched the review UI on. Rather than a
    // moderation surface nobody meant to ship, the route simply does not
    // exist — the same honest absence a disabled feature gets anywhere
    // else in this app.
    notFound();
  }

  const { sla } = await searchParams;
  const activeSla = SLA_CLASSES.includes(sla as SlaClass) ? (sla as SlaClass) : undefined;

  let items: ReviewItem[];
  let unreachable = false;
  let disabledOnServer = false;
  try {
    const page = await listReviewItems({ sla: activeSla ? [activeSla] : undefined, limit: 100 });
    items = page.items;
  } catch (error) {
    if (isUnreachable(error)) {
      unreachable = true;
    } else {
      // Most likely: FIRMSCOUT_REVIEW_UI_ENABLED is on for this app but
      // FIRMSCOUT_REVIEW_API_ENABLED is off on the server, so the route
      // was never registered. Those are two of the three switches that
      // bear on this surface — this app's, and the API server's pair —
      // which api.md §11 tabulates. Say so rather than presenting a
      // generic failure.
      disabledOnServer = true;
    }
    items = [];
  }

  return (
    <div className="mx-auto max-w-6xl px-4 py-12 sm:px-6">
      <UnauthenticatedBanner />

      <h1 className="text-2xl font-bold tracking-tight text-ink-950">Review queue</h1>
      <p className="mt-2 text-sm text-ink-500">
        Candidates a validation gate could not clear automatically, highest priority first. Age is
        not part of the score — within one priority band the oldest item already sorts first.
      </p>

      <nav aria-label="Filter by SLA class" className="mt-6 flex flex-wrap items-center gap-2 text-sm">
        <span className="text-ink-500">SLA:</span>
        <Link href="/review">
          <Badge variant={activeSla ? "outline" : "default"}>All</Badge>
        </Link>
        {SLA_CLASSES.map((cls) => (
          <Link key={cls} href={`/review?sla=${cls}`}>
            <Badge variant={activeSla === cls ? "default" : "outline"}>{cls}</Badge>
          </Link>
        ))}
      </nav>

      <div className="mt-6">
        {unreachable ? (
          <Unavailable
            title="Review queue temporarily unavailable"
            detail="We couldn't reach the FirmScout API just now. Please try again shortly."
          />
        ) : disabledOnServer ? (
          <Unavailable
            title="The review API is not enabled on this deployment"
            detail="FIRMSCOUT_REVIEW_API_ENABLED (or its use-case wiring) is off on the server this
              app is pointed at. That switch defaults to false by design (ADR-0021); an operator
              must turn it on deliberately."
          />
        ) : items.length === 0 ? (
          <p className="text-sm text-ink-500">
            The review queue is empty — nothing is waiting on a human decision right now.
          </p>
        ) : (
          <Table>
            <caption className="sr-only">Pending review items, highest priority first</caption>
            <TableHeader>
              <TableRow>
                <TableHead>Item</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>SLA</TableHead>
                <TableHead>Priority</TableHead>
                <TableHead>Vendor</TableHead>
                <TableHead>Product</TableHead>
                <TableHead>Age</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((item) => (
                <QueueRow key={item.id} item={item} />
              ))}
            </TableBody>
          </Table>
        )}
      </div>
    </div>
  );
}
