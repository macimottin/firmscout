import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { ReleaseDate } from "@/components/release-date";
import { Unavailable } from "@/components/unavailable";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  getLatestRelease,
  getProduct,
  isNotFound,
  isUnreachable,
  listProductReleases,
  type Product,
  type Release,
} from "@/lib/api";

export const revalidate = 3600;

interface ProductPageProps {
  params: Promise<{ slug: string }>;
}

export async function generateMetadata({ params }: ProductPageProps): Promise<Metadata> {
  const { slug } = await params;
  try {
    const product = await getProduct(slug);
    const versionPart = product.latestRelease
      ? ` — latest observed version ${product.latestRelease.rawVersion}`
      : "";
    return {
      title: `${product.name} (${product.vendor.name})`,
      description: `${product.vendor.name} ${product.name} version history and release metadata, sourced from official vendor publications${versionPart}.`,
      alternates: { canonical: `/products/${product.slug}` },
    };
  } catch {
    return { title: "Product" };
  }
}

function sourceQualityLabel(quality: Product["sourceQuality"]): string {
  switch (quality) {
    case "official_primary":
      return "Official primary source";
    case "official_secondary":
      return "Official secondary source";
    case "community":
      return "Community-contributed source";
    default:
      return "Not yet assessed";
  }
}

function eolStatusLabel(status: NonNullable<Product["eol"]>["status"]): { label: string; variant: "success" | "warning" | "danger" | "default" } {
  switch (status) {
    case "supported":
      return { label: "Supported", variant: "success" };
    case "eol":
      return { label: "End of life", variant: "danger" };
    case "eos":
      return { label: "End of sale", variant: "warning" };
    default:
      return { label: "Unknown", variant: "default" };
  }
}

export default async function ProductPage({ params }: ProductPageProps) {
  const { slug } = await params;

  let product: Product;
  try {
    product = await getProduct(slug);
  } catch (error) {
    if (isNotFound(error)) notFound();
    return (
      <div className="mx-auto max-w-4xl px-4 py-12 sm:px-6">
        <h1 className="text-2xl font-bold tracking-tight text-ink-950">Product</h1>
        <div className="mt-6">
          <Unavailable />
        </div>
      </div>
    );
  }

  const [latestResult, historyResult] = await Promise.allSettled([
    getLatestRelease(product.slug),
    listProductReleases(product.slug, { limit: 25 }),
  ]);

  const latestRelease: Release | null =
    latestResult.status === "fulfilled" ? latestResult.value.latestRelease : null;
  const latestUnavailable = latestResult.status === "rejected" && isUnreachable(latestResult.reason);

  const history = historyResult.status === "fulfilled" ? historyResult.value.releases : [];
  const historyUnavailable = historyResult.status === "rejected" && isUnreachable(historyResult.reason);

  const jsonLd = buildJsonLd(product, latestRelease);

  return (
    <div className="mx-auto max-w-4xl px-4 py-12 sm:px-6">
      <script
        type="application/ld+json"
        // JSON-LD built entirely from this page's own typed API data (no
        // user input); "<" is escaped so no value can prematurely close
        // the script tag.
        dangerouslySetInnerHTML={{ __html: JSON.stringify(jsonLd).replace(/</g, "\\u003c") }}
      />

      <p className="text-sm text-ink-500">
        <Link href="/vendors" className="hover:underline">
          Vendors
        </Link>{" "}
        /{" "}
        <Link href={`/vendors/${product.vendor.slug}`} className="hover:underline">
          {product.vendor.name}
        </Link>
      </p>
      <h1 className="mt-1 text-2xl font-bold tracking-tight text-ink-950">{product.name}</h1>

      {product.hasSourceConflict ? (
        <div className="mt-4">
          <Alert variant="danger">
            <AlertTitle>Conflicting sources</AlertTitle>
            <AlertDescription>
              Two or more official sources disagree about this product&apos;s current facts.
              FirmScout does not auto-resolve source conflicts — the values below are shown as
              published, and a human review is pending.
            </AlertDescription>
          </Alert>
        </div>
      ) : null}

      <dl className="mt-6 grid grid-cols-1 gap-x-6 gap-y-4 text-sm sm:grid-cols-2">
        <div>
          <dt className="text-ink-500">Vendor</dt>
          <dd className="mt-0.5 text-ink-900">
            <Link href={`/vendors/${product.vendor.slug}`} className="hover:underline">
              {product.vendor.name}
            </Link>
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Family</dt>
          <dd className="mt-0.5 text-ink-900">
            {product.family ? (
              <Link href={`/products/${product.family.slug}`} className="hover:underline">
                {product.family.name}
              </Link>
            ) : (
              "Not part of a product family"
            )}
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Aliases</dt>
          <dd className="mt-0.5 text-ink-900">
            {product.aliases && product.aliases.length > 0
              ? product.aliases.join(", ")
              : "No aliases recorded"}
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Category</dt>
          <dd className="mt-0.5 text-ink-900">{product.category}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Release type</dt>
          <dd className="mt-0.5 text-ink-900">{product.releaseType}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Source quality</dt>
          <dd className="mt-0.5 text-ink-900">{sourceQualityLabel(product.sourceQuality)}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Last verified</dt>
          <dd className="mt-0.5 text-ink-900">
            {new Date(product.lastVerifiedAt).toISOString().replace("T", " ").slice(0, 16)} UTC
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Lifecycle status</dt>
          <dd className="mt-0.5 text-ink-900">
            {product.eol ? (
              <span className="inline-flex items-center gap-2">
                <Badge variant={eolStatusLabel(product.eol.status).variant}>
                  {eolStatusLabel(product.eol.status).label}
                </Badge>
                {product.eol.eolDate ? (
                  <ReleaseDate
                    date={product.eol.eolDate}
                    precision={product.eol.eolDatePrecision ?? "unknown"}
                  />
                ) : null}
              </span>
            ) : (
              "Lifecycle status not yet tracked for this product"
            )}
          </dd>
        </div>
      </dl>

      <section className="mt-10" aria-labelledby="latest-heading">
        <h2 id="latest-heading" className="text-lg font-semibold text-ink-950">
          Latest release
        </h2>
        <div className="mt-4">
          {latestUnavailable ? (
            <Unavailable
              title="Latest release temporarily unavailable"
              detail="We couldn't load the latest release just now. Please try again shortly."
            />
          ) : !latestRelease ? (
            <p className="text-sm text-ink-500">No release has been published for this product yet.</p>
          ) : (
            <div className="rounded-lg border border-ink-200 bg-white p-5">
              <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                <span className="font-mono text-xl font-semibold text-ink-950">
                  {latestRelease.rawVersion}
                </span>
                <Badge variant="outline">{latestRelease.channel}</Badge>
                {latestRelease.withdrawn ? <Badge variant="danger">Withdrawn</Badge> : null}
              </div>
              <dl className="mt-4 grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
                <div>
                  <dt className="text-ink-500">Latest observed version</dt>
                  <dd className="mt-0.5 font-mono text-ink-900">{latestRelease.rawVersion}</dd>
                </div>
                <div>
                  <dt className="text-ink-500">Latest recommended version</dt>
                  <dd className="mt-0.5 text-ink-900">
                    {latestRelease.recommended === true ? (
                      <span className="font-mono">{latestRelease.rawVersion}</span>
                    ) : (
                      "The vendor has not designated a recommended version"
                    )}
                  </dd>
                </div>
                <div>
                  <dt className="text-ink-500">Release date</dt>
                  <dd className="mt-0.5 text-ink-900">
                    <ReleaseDate
                      date={latestRelease.releaseDate}
                      precision={latestRelease.releaseDatePrecision}
                    />
                  </dd>
                </div>
                <div>
                  <dt className="text-ink-500">Last verified</dt>
                  <dd className="mt-0.5 text-ink-900">
                    {new Date(latestRelease.lastVerifiedAt).toISOString().replace("T", " ").slice(0, 16)} UTC
                  </dd>
                </div>
              </dl>
              <div className="mt-4 border-t border-ink-100 pt-4 text-sm">
                <p className="text-ink-500">
                  Evidence retrieved{" "}
                  {new Date(latestRelease.evidence.retrievedAt).toISOString().slice(0, 10)} from{" "}
                  <a
                    href={latestRelease.source.url}
                    rel="noopener noreferrer"
                    target="_blank"
                    className="text-ink-800 underline-offset-2 hover:underline"
                  >
                    {latestRelease.source.official ? "official source" : "source"}
                  </a>
                  :
                </p>
                <p className="mt-1 rounded bg-ink-50 p-3 font-mono text-xs text-ink-700">
                  &ldquo;{latestRelease.evidence.excerpt}&rdquo;
                </p>
              </div>
            </div>
          )}
        </div>
      </section>

      <section className="mt-10" aria-labelledby="history-heading">
        <h2 id="history-heading" className="text-lg font-semibold text-ink-950">
          Version history
        </h2>
        <div className="mt-4">
          {historyUnavailable ? (
            <Unavailable
              title="Version history temporarily unavailable"
              detail="We couldn't load past releases just now. Please try again shortly."
            />
          ) : history.length === 0 ? (
            <p className="text-sm text-ink-500">No prior releases are on record.</p>
          ) : (
            <Table>
              <caption className="sr-only">Version history for {product.name}</caption>
              <TableHeader>
                <TableRow>
                  <TableHead>Version</TableHead>
                  <TableHead>Channel</TableHead>
                  <TableHead>Release date</TableHead>
                  <TableHead>Recommended</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {history.map((release) => (
                  <TableRow key={release.id}>
                    <TableCell>
                      <Link
                        href={`/releases/${release.id}`}
                        className="font-mono text-ink-900 underline-offset-2 hover:underline"
                      >
                        {release.rawVersion}
                      </Link>
                    </TableCell>
                    <TableCell className="text-ink-600">{release.channel}</TableCell>
                    <TableCell className="text-ink-600">
                      <ReleaseDate date={release.releaseDate} precision={release.releaseDatePrecision} />
                    </TableCell>
                    <TableCell className="text-ink-600">
                      {release.recommended === true ? "Yes" : "—"}
                    </TableCell>
                    <TableCell className="text-ink-600">
                      {release.withdrawn ? <Badge variant="danger">Withdrawn</Badge> : "Published"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      </section>

      <section className="mt-10" aria-labelledby="advisories-heading">
        <h2 id="advisories-heading" className="text-lg font-semibold text-ink-950">
          Security advisories
        </h2>
        <div className="mt-4">
          <p className="text-sm text-ink-500">
            No security advisories are on record for this product. CVE correlation is a designed,
            not yet built, part of FirmScout — see the project&apos;s{" "}
            <Link href="/status" className="hover:underline">
              status page
            </Link>
            .
          </p>
        </div>
      </section>

      <section className="mt-10" aria-labelledby="sources-heading">
        <h2 id="sources-heading" className="text-lg font-semibold text-ink-950">
          Official sources
        </h2>
        <ul className="mt-4 space-y-2 text-sm">
          {product.officialSources.length === 0 ? (
            <li className="text-ink-500">No official sources recorded.</li>
          ) : (
            product.officialSources.map((source) => (
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
      </section>
    </div>
  );
}

function buildJsonLd(product: Product, latestRelease: Release | null) {
  const jsonLd: Record<string, unknown> = {
    "@context": "https://schema.org",
    "@type": "SoftwareApplication",
    name: product.name,
    applicationCategory: product.category,
    manufacturer: {
      "@type": "Organization",
      name: product.vendor.name,
    },
  };

  if (latestRelease) {
    jsonLd.softwareVersion = latestRelease.rawVersion;
    // Only ever include a machine-readable date when the vendor published
    // day-level precision — never invent day/month components here either.
    if (latestRelease.releaseDatePrecision === "exact_day" && latestRelease.releaseDate) {
      jsonLd.datePublished = latestRelease.releaseDate;
    }
  }

  if (product.officialSources.length > 0) {
    jsonLd.url = product.officialSources[0]?.url;
  }

  return jsonLd;
}
