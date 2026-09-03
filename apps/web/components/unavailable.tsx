import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";

/**
 * The calm, honest "catalogue data is temporarily unavailable" state every
 * page falls back to when the FirmScout API cannot be reached. Never a
 * stack trace, never a crash — the API being down is an expected,
 * anticipated condition, not an application error.
 */
export function Unavailable({
  title = "Catalogue data is temporarily unavailable",
  detail = "We couldn't reach the FirmScout API just now. This is usually temporary — please try again in a moment.",
}: {
  title?: string;
  detail?: string;
}) {
  return (
    <Alert variant="warning">
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription>{detail}</AlertDescription>
    </Alert>
  );
}
