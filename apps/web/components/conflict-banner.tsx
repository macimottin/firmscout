import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import type { SourceConflictDetail } from "@/lib/api";
import { describeConflict } from "@/lib/conflict";

/**
 * The "Conflicting sources" alert on a product page. `detail` is optional and
 * nullable on purpose (see `SourceConflictDetail` in lib/api.ts): a product summary
 * computed before this field existed, or a conflict flagged but not yet detailed,
 * must render the same generic banner as today, not "undefined" and not a crash.
 * All of the actual fallback logic lives in `describeConflict` (lib/conflict.ts, unit
 * tested) so this component stays a thin, obviously-correct binding.
 */
export function ConflictBanner({ detail }: { detail?: SourceConflictDetail | null }) {
  const { title, description } = describeConflict(detail);
  return (
    <div className="mt-4">
      <Alert variant="danger">
        <AlertTitle>{title}</AlertTitle>
        <AlertDescription>{description}</AlertDescription>
      </Alert>
    </div>
  );
}
