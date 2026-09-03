import type { Metadata } from "next";
import Link from "next/link";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { Unavailable } from "@/components/unavailable";
import { isUnreachable, listVendors } from "@/lib/api";

export const metadata: Metadata = {
  title: "Vendors",
  description: "Browse every vendor in FirmScout's catalogue.",
  alternates: { canonical: "/vendors" },
};

export const revalidate = 3600;

export default async function VendorsPage() {
  let vendors;
  try {
    const response = await listVendors({ limit: 200 });
    vendors = response.vendors;
  } catch (error) {
    return (
      <div className="mx-auto max-w-5xl px-4 py-12 sm:px-6">
        <h1 className="text-2xl font-bold tracking-tight text-ink-950">Vendors</h1>
        <div className="mt-6">{isUnreachable(error) ? <Unavailable /> : <Unavailable title="Vendors could not be loaded" detail="Please try again shortly." />}</div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-5xl px-4 py-12 sm:px-6">
      <h1 className="text-2xl font-bold tracking-tight text-ink-950">Vendors</h1>
      <p className="mt-2 text-sm text-ink-500">
        {vendors.length} vendor{vendors.length === 1 ? "" : "s"} in the catalogue so far.
      </p>

      {vendors.length === 0 ? (
        <p className="mt-8 text-sm text-ink-500">No vendors are published yet.</p>
      ) : (
        <ul className="mt-8 grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {vendors.map((vendor) => (
            <li key={vendor.slug}>
              <Link href={`/vendors/${vendor.slug}`} className="block h-full">
                <Card className="h-full transition-colors hover:border-ink-400">
                  <CardHeader>
                    <CardTitle>{vendor.name}</CardTitle>
                    <CardDescription>
                      {vendor.productCount} product{vendor.productCount === 1 ? "" : "s"}
                    </CardDescription>
                  </CardHeader>
                  <CardContent className="text-sm text-ink-500">{vendor.website}</CardContent>
                </Card>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
