import { Badge } from "@/components/ui/badge";
import type { OfficialSource } from "@/lib/api";

/**
 * The product page's "Official sources" list: the distinct sources that have
 * actually contributed a currently-mapped, non-withdrawn release to this product
 * (the API's job to filter that way, not this component's -- see `Product.officialSources`
 * in lib/api.ts). Extracted out of the page component so it can be exercised directly
 * in official-sources-list.test.ts without needing a running page.
 */
export function OfficialSourcesList({ sources }: { sources: OfficialSource[] }) {
  return (
    <ul className="mt-4 space-y-2 text-sm">
      {sources.length === 0 ? (
        <li className="text-ink-500">No official sources recorded.</li>
      ) : (
        sources.map((source) => (
          <li key={source.url}>
            <a
              href={source.url}
              rel="noopener noreferrer"
              target="_blank"
              className="text-ink-800 underline-offset-2 hover:underline"
            >
              {source.url}
            </a>
            {source.official ? (
              <Badge variant="success" className="ml-2">
                Official
              </Badge>
            ) : null}
          </li>
        ))
      )}
    </ul>
  );
}
