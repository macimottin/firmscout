import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Unavailable } from "@/components/unavailable";
import {
  getVendor,
  isNotFound,
  search,
  type SearchResultItem,
} from "@/lib/api";
import { formatUtcDay } from "@/lib/release-display";
import { productsListedCount } from "@/lib/vendor-products";

export const revalidate = 3600;

interface VendorPageProps {
  params: Promise<{ slug: string }>;
}

export async function generateMetadata({
  params,
}: VendorPageProps): Promise<Metadata> {
  const { slug } = await params;
  try {
    const vendor = await getVendor(slug);
    // `vendor.productCount` is never sent by the vendor read model today (see its
    // doc comment in lib/api.ts) -- interpolating it unconditionally rendered the
    // literal word "undefined" into a live <meta description>, in every search
    // engine's index of every vendor page. There is no honest count to put here
    // instead (generateMetadata has no access to the page body's search-derived
    // product list, and would not want to shoulder its own API call just to
    // describe one), so the sentence simply does not claim a count.
    return {
      title: vendor.name,
      description: `${vendor.name}'s products in FirmScout's version metadata catalogue.`,
      alternates: { canonical: `/vendors/${vendor.slug}` },
    };
  } catch {
    return { title: "Vendor" };
  }
}

/**
 * The documented API (docs/architecture/api.md §2) has no endpoint that
 * lists a vendor's products directly — only `/products/{slug}` (one
 * product) and `/search` exist. As a best-effort substitute, pending a
 * dedicated `/vendors/{slug}/products`-style endpoint, this searches for
 * the vendor's own name and keeps only product results attributed to this
 * vendor slug. It is a workaround, not invented data: every row shown
 * still comes from the API's own search response.
 */
async function findVendorProducts(vendorSlug: string, vendorName: string) {
  try {
    const response = await search(vendorName, { limit: 50 });
    return response.results.filter(
      (
        item,
      ): item is SearchResultItem & {
        vendor: { slug: string; name: string };
      } => item.type === "product" && item.vendor?.slug === vendorSlug,
    );
  } catch {
    return null;
  }
}

export default async function VendorPage({ params }: VendorPageProps) {
  const { slug } = await params;

  let vendor;
  try {
    vendor = await getVendor(slug);
  } catch (error) {
    if (isNotFound(error)) notFound();
    return (
      <div className="mx-auto max-w-4xl px-4 py-12 sm:px-6">
        <h1 className="text-2xl font-bold tracking-tight text-ink-950">
          Vendor
        </h1>
        <div className="mt-6">
          <Unavailable />
        </div>
      </div>
    );
  }

  const products = await findVendorProducts(vendor.slug, vendor.name);

  return (
    <div className="mx-auto max-w-4xl px-4 py-12 sm:px-6">
      <p className="text-sm text-ink-500">
        <Link href="/vendors" className="hover:underline">
          Vendors
        </Link>
      </p>
      <h1 className="mt-1 text-2xl font-bold tracking-tight text-ink-950">
        {vendor.name}
      </h1>
      <dl className="mt-4 grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
        <div>
          <dt className="text-ink-500">Website</dt>
          <dd className="mt-0.5">
            <a
              href={vendor.website}
              rel="noopener noreferrer"
              target="_blank"
              className="text-ink-800 underline-offset-2 hover:underline"
            >
              {vendor.website}
            </a>
          </dd>
        </div>
        {/* DEFECT 13: `vendor.productCount` is never sent by the vendor read model
            (see lib/api.ts), so this row always rendered the fixed string "Not
            recorded" -- directly above the list of products the page renders below
            it, which is never empty when this vendor has any. That was true of the
            response and false of the viewport: a page contradicting itself in one
            screen is still a defect even when every individual sentence is
            accurate. There is no honest aggregate to substitute (`products` is
            itself a best-effort search match, not a verified total -- see
            findVendorProducts's doc comment), so this deliberately stops claiming a
            vendor-wide count and instead reports the one number this page can state
            without inventing anything: how many rows are in the list right below
            it. See lib/vendor-products.ts. */}
        <div>
          <dt className="text-ink-500">Products listed below</dt>
          <dd className="mt-0.5 text-ink-800">
            {productsListedCount(products)}
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Last verified</dt>
          <dd className="mt-0.5 text-ink-800">
            {formatUtcDay(vendor.lastVerifiedAt) ?? "Not recorded"}
          </dd>
        </div>
      </dl>

      <h2 className="mt-10 text-lg font-semibold text-ink-950">Products</h2>
      <div className="mt-4">
        {products === null ? (
          <Unavailable
            title="Product list temporarily unavailable"
            detail="We couldn't load this vendor's products just now. Please try again shortly."
          />
        ) : products.length === 0 ? (
          <p className="text-sm text-ink-500">
            No products found for {vendor.name} yet, even though the vendor is
            registered.
          </p>
        ) : (
          <ul className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            {products.map((product) => (
              <li key={product.slug}>
                <Link
                  href={`/products/${product.slug}`}
                  className="block h-full"
                >
                  <Card className="h-full transition-colors hover:border-ink-400">
                    <CardHeader>
                      <CardTitle>{product.name}</CardTitle>
                    </CardHeader>
                    <CardContent className="text-sm text-ink-500">
                      View product details
                    </CardContent>
                  </Card>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
