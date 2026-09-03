import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Unavailable } from "@/components/unavailable";
import { getVendor, isNotFound, search, type SearchResultItem } from "@/lib/api";

export const revalidate = 3600;

interface VendorPageProps {
  params: Promise<{ slug: string }>;
}

export async function generateMetadata({ params }: VendorPageProps): Promise<Metadata> {
  const { slug } = await params;
  try {
    const vendor = await getVendor(slug);
    return {
      title: vendor.name,
      description: `${vendor.name}'s products in FirmScout's version metadata catalogue, with ${vendor.productCount} tracked product${vendor.productCount === 1 ? "" : "s"}.`,
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
      (item): item is SearchResultItem & { vendor: { slug: string; name: string } } =>
        item.type === "product" && item.vendor?.slug === vendorSlug,
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
        <h1 className="text-2xl font-bold tracking-tight text-ink-950">Vendor</h1>
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
      <h1 className="mt-1 text-2xl font-bold tracking-tight text-ink-950">{vendor.name}</h1>
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
        <div>
          <dt className="text-ink-500">Products tracked</dt>
          <dd className="mt-0.5 text-ink-800">{vendor.productCount}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Last verified</dt>
          <dd className="mt-0.5 text-ink-800">
            {new Date(vendor.lastVerifiedAt).toISOString().slice(0, 10)}
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
            No products found for {vendor.name} yet, even though the vendor is registered.
          </p>
        ) : (
          <ul className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            {products.map((product) => (
              <li key={product.slug}>
                <Link href={`/products/${product.slug}`} className="block h-full">
                  <Card className="h-full transition-colors hover:border-ink-400">
                    <CardHeader>
                      <CardTitle>{product.name}</CardTitle>
                    </CardHeader>
                    <CardContent className="text-sm text-ink-500">View product details</CardContent>
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
