import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import type { Product } from "@/lib/api";
import { describeApplicability } from "@/lib/applicability";

/**
 * The visible caveat that FirmScout has not verified which releases apply to this
 * exact model (ADR-0024) -- rendered with the same `Alert` primitive
 * `ConflictBanner` uses for the same reason: a fleet manager acting on an
 * unverified applicability claim is the harm this discipline exists to prevent,
 * so the notice belongs where it cannot be missed, not folded into a caption.
 *
 * Renders nothing, not an empty box, when `describeApplicability` returns null --
 * that is the "own_releases" case, where today's page is already correct.
 *
 * `observedRelease` must match whether the page is about to render a latest
 * release: product and /latest are separate fetches and can disagree on cache.
 */
export function ApplicabilityAlert({
  product,
  observedRelease = false,
}: {
  product: Product;
  observedRelease?: boolean;
}) {
  const notice = describeApplicability(product, { observedRelease });
  if (!notice) return null;
  return (
    <div className="mt-4">
      <Alert variant="warning">
        <AlertTitle>{notice.title}</AlertTitle>
        <AlertDescription>{notice.description}</AlertDescription>
      </Alert>
    </div>
  );
}
