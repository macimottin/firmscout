import type { MetadataRoute } from "next";
import { listVendors } from "@/lib/api";

const siteUrl = (process.env.NEXT_PUBLIC_SITE_URL ?? "https://firmscout.dev").replace(/\/+$/, "");

export default async function sitemap(): Promise<MetadataRoute.Sitemap> {
  const staticEntries: MetadataRoute.Sitemap = [
    { url: `${siteUrl}/`, changeFrequency: "daily", priority: 1 },
    { url: `${siteUrl}/vendors`, changeFrequency: "daily", priority: 0.8 },
    { url: `${siteUrl}/search`, changeFrequency: "weekly", priority: 0.3 },
    { url: `${siteUrl}/status`, changeFrequency: "hourly", priority: 0.2 },
  ];

  // Best-effort: the catalogue API may be unreachable at build/request
  // time. A sitemap missing dynamic entries is far better than a build
  // that fails outright.
  try {
    const { vendors } = await listVendors({ limit: 200 });
    const vendorEntries: MetadataRoute.Sitemap = vendors.map((vendor) => ({
      url: `${siteUrl}/vendors/${vendor.slug}`,
      lastModified: vendor.lastVerifiedAt,
      changeFrequency: "daily",
      priority: 0.6,
    }));
    return [...staticEntries, ...vendorEntries];
  } catch {
    return staticEntries;
  }
}
