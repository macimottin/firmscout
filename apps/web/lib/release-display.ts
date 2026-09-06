/**
 * Defensive accessors for a release's `source` and `evidence`.
 *
 * The API contract guarantees both are present on every release
 * (`releases.evidence_id` is `NOT NULL REFERENCES evidence`), so `lib/api.ts` types
 * them as required, not optional -- that guarantee is real and this file does not
 * relitigate it by weakening the type. But a type only binds new API responses; it
 * says nothing about a stale ISR cache entry, a response cached before this field
 * existed, or a future contract change reaching a client built against today's
 * types. Rendering code that trusts the type completely and dereferences straight
 * through is exactly the class of bug that shipped release pages a 500 (see the
 * release detail page). Every function here treats its input as possibly missing
 * regardless of what the type promises, so a caller can guard without re-deriving
 * this logic at every call site.
 */

import type { Evidence, ReleaseSource } from "./api";

/** "official source" / "source" for the link text; never throws on a missing source. */
export function sourceLabel(source: ReleaseSource | null | undefined): string {
  return source?.official ? "official source" : "source";
}

/** The link target, or null when there is nothing honest to link to. */
export function sourceUrl(
  source: ReleaseSource | null | undefined,
): string | null {
  return source?.url ? source.url : null;
}

/** The verbatim excerpt, or null rather than an empty string standing in for "missing". */
export function evidenceExcerpt(
  evidence: Evidence | null | undefined,
): string | null {
  return evidence?.excerpt ? evidence.excerpt : null;
}

/**
 * The parsed retrieval instant, or null for a missing or unparseable timestamp.
 * Returned as a `Date` rather than a pre-formatted string because the two call sites
 * (product page, release page) render it at different granularities.
 */
export function parsedEvidenceRetrievedAt(
  evidence: Evidence | null | undefined,
): Date | null {
  const raw = evidence?.retrievedAt;
  if (!raw) return null;
  const parsed = new Date(raw);
  return Number.isNaN(parsed.getTime()) ? null : parsed;
}

/**
 * A UTC instant rendered to the minute (`YYYY-MM-DD HH:MM`), or null when there is
 * nothing honest to render.
 *
 * `new Date(undefined).toISOString()` does not return a placeholder, it throws
 * `RangeError: Invalid time value`, and in a server component that is a 500 for the
 * whole page rather than one blank cell. That is not hypothetical here: `lastVerifiedAt`
 * is `omitempty` on the wire and a hardware model has no verifying source, so the first
 * device added to the catalogue turned the product page and the search page into 500s.
 * The guard belongs in one tested function rather than at each call site, because the
 * next always-present-until-it-wasn't field will arrive the same way.
 */
export function formatUtcMinute(raw: string | null | undefined): string | null {
  const parsed = parseInstant(raw);
  return parsed === null
    ? null
    : parsed.toISOString().replace("T", " ").slice(0, 16);
}

/** The same guard at day granularity (`YYYY-MM-DD`), for table cells too narrow for a time. */
export function formatUtcDay(raw: string | null | undefined): string | null {
  const parsed = parseInstant(raw);
  return parsed === null ? null : parsed.toISOString().slice(0, 10);
}

function parseInstant(raw: string | null | undefined): Date | null {
  if (!raw) return null;
  const parsed = new Date(raw);
  return Number.isNaN(parsed.getTime()) ? null : parsed;
}
