import { Badge } from "@/components/ui/badge";
import type { Evidence, ReleaseSource } from "@/lib/api";
import { evidenceExcerpt, parsedEvidenceRetrievedAt, sourceUrl } from "@/lib/release-display";

/**
 * The release detail page's "Source and evidence" section -- the block that was
 * crashing the page in production because the backend never populated `source` /
 * `evidence` (see PresentRelease). The backend guarantee is real now
 * (`releases.evidence_id` is `NOT NULL`), but this component still treats both props
 * as possibly missing: a stale ISR cache entry, or a response cached before the
 * backend fix landed, is not bound by today's guarantee, and a page that trusts a
 * required field completely is exactly the bug this component exists to not repeat.
 * `source`/`evidence` are typed required on `Release` itself (lib/api.ts) -- the
 * looser types here are this component's own defense, not a claim that the API can
 * legitimately omit them.
 */
export function ReleaseSourceEvidence({
  source,
  evidence,
}: {
  source?: ReleaseSource | null;
  evidence?: Evidence | null;
}) {
  const url = sourceUrl(source);
  const retrievedAt = parsedEvidenceRetrievedAt(evidence);
  const excerpt = evidenceExcerpt(evidence);

  return (
    <div className="mt-4 rounded-lg border border-ink-200 bg-white p-5 text-sm">
      <p className="text-ink-600">
        {url ? (
          <a
            href={url}
            rel="noopener noreferrer"
            target="_blank"
            className="text-ink-800 underline-offset-2 hover:underline"
          >
            {url}
          </a>
        ) : (
          <span className="text-ink-500">Source not recorded</span>
        )}{" "}
        {source ? (
          source.official ? (
            <Badge variant="success" className="ml-1">
              Official
            </Badge>
          ) : (
            <Badge variant="outline" className="ml-1">
              Unofficial
            </Badge>
          )
        ) : null}
      </p>
      <p className="mt-3 text-ink-500">
        {retrievedAt
          ? `Retrieved ${retrievedAt.toISOString().replace("T", " ").slice(0, 16)} UTC`
          : "Retrieval time not recorded"}
      </p>
      <p className="mt-2 rounded bg-ink-50 p-3 font-mono text-xs text-ink-700">
        {excerpt ? `“${excerpt}”` : "No evidence excerpt recorded."}
      </p>
    </div>
  );
}
