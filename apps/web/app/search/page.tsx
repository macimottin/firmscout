import type { Metadata } from "next";
import Link from "next/link";
import { SearchForm } from "@/components/search-form";
import { ReleaseDate } from "@/components/release-date";
import { Unavailable } from "@/components/unavailable";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { getProduct, isUnreachable, search, type Product, type SearchResultItem } from "@/lib/api";

export const metadata: Metadata = {
  title: "Search",
  description: "Search FirmScout's catalogue of vendors and products.",
};

interface SearchPageProps {
  searchParams: Promise<{ q?: string }>;
}

async function enrichProductResult(item: SearchResultItem): Promise<Product | null> {
  try {
    return await getProduct(item.slug);
  } catch {
    return null;
  }
}

export default async function SearchPage({ searchParams }: SearchPageProps) {
  const { q = "" } = await searchParams;
  const query = q.trim();

  return (
    <div className="mx-auto max-w-4xl px-4 py-12 sm:px-6">
      <h1 className="text-2xl font-bold tracking-tight text-ink-950">Search</h1>
      <div className="mt-6">
        <SearchForm defaultValue={query} />
      </div>

      <div className="mt-8">
        {query.length === 0 ? (
          <p className="text-sm text-ink-500">Enter a vendor or product name above to search.</p>
        ) : (
          <SearchResults query={query} />
        )}
      </div>
    </div>
  );
}

async function SearchResults({ query }: { query: string }) {
  let results: SearchResultItem[] = [];
  try {
    const response = await search(query, { limit: 50 });
    results = response.results;
  } catch (error) {
    if (isUnreachable(error)) {
      return <Unavailable />;
    }
    return (
      <p className="text-sm text-ink-500">
        Something went wrong running that search. Please try again.
      </p>
    );
  }

  if (results.length === 0) {
    return (
      <p className="text-sm text-ink-500">
        No vendors or products matched &ldquo;{query}&rdquo;. Try a different spelling, or the
        vendor name on its own.
      </p>
    );
  }

  const productResults = results.filter((r) => r.type === "product");
  const enriched = await Promise.all(productResults.map((item) => enrichProductResult(item)));
  const productDetails = new Map(productResults.map((item, i) => [item.slug, enriched[i]]));

  return (
    <Table>
      <caption className="sr-only">
        Search results for &ldquo;{query}&rdquo;
      </caption>
      <TableHeader>
        <TableRow>
          <TableHead>Result</TableHead>
          <TableHead>Vendor</TableHead>
          <TableHead>Latest version</TableHead>
          <TableHead>Release date</TableHead>
          <TableHead>Release type</TableHead>
          <TableHead>Last verified</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {results.map((item) => {
          if (item.type === "vendor") {
            return (
              <TableRow key={`vendor-${item.slug}`}>
                <TableCell>
                  <Link
                    href={`/vendors/${item.slug}`}
                    className="font-medium text-ink-950 underline-offset-2 hover:underline"
                  >
                    {item.name}
                  </Link>
                  <Badge variant="outline" className="ml-2">
                    Vendor
                  </Badge>
                </TableCell>
                <TableCell colSpan={5} className="text-ink-500">
                  Matched on {item.matchedOn}
                </TableCell>
              </TableRow>
            );
          }

          const product = productDetails.get(item.slug) ?? null;
          const latest = product?.latestRelease ?? null;

          return (
            <TableRow key={`product-${item.slug}`}>
              <TableCell>
                <Link
                  href={`/products/${item.slug}`}
                  className="font-medium text-ink-950 underline-offset-2 hover:underline"
                >
                  {item.name}
                </Link>
              </TableCell>
              <TableCell className="text-ink-600">
                {item.vendor ? (
                  <Link href={`/vendors/${item.vendor.slug}`} className="hover:underline">
                    {item.vendor.name}
                  </Link>
                ) : (
                  "—"
                )}
              </TableCell>
              <TableCell className="font-mono text-ink-800">
                {latest ? latest.rawVersion : product === null ? "—" : "Unknown"}
              </TableCell>
              <TableCell className="text-ink-600">
                {latest ? (
                  <ReleaseDate date={latest.releaseDate} precision={latest.releaseDatePrecision} />
                ) : (
                  "—"
                )}
              </TableCell>
              <TableCell className="text-ink-600">{product?.releaseType ?? "—"}</TableCell>
              <TableCell className="text-ink-500">
                {product ? new Date(product.lastVerifiedAt).toISOString().slice(0, 10) : "—"}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
