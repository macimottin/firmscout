import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";

/**
 * The honesty requirement this whole surface exists under (ADR-0021): the
 * review queue has no login. These pages are read-only — they can no
 * longer accept or reject anything (that write only happens from the CLI
 * now, run by an operator holding a database connection string) — but
 * they still show internal detail that was never meant for a public
 * audience: candidate provenance and evidence excerpts, priority
 * scoring, and the audit trail's asserted (unverified) reviewer names.
 * This banner is what stops that from reading as a public page just
 * because nothing on it writes any more. It appears on every review
 * page.
 */
export function UnauthenticatedBanner() {
  return (
    <Alert variant="danger" className="mb-6">
      <AlertTitle>This is a read-only, unauthenticated internal view</AlertTitle>
      <AlertDescription>
        Anyone who can reach this page can see the review queue, its evidence and its audit
        trail — FirmScout has no login yet (ADR-0021). It can no longer accept or reject
        anything; that decision is made only from the FirmScout CLI, against the database
        directly. This deployment should still not be reachable from the public internet: what
        remains is a lower-severity information disclosure, not an open write, but it is not
        nothing.
      </AlertDescription>
    </Alert>
  );
}
