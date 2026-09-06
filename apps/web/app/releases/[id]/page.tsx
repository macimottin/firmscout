import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { ReleaseDate } from "@/components/release-date";
import { ReleaseSourceEvidence } from "@/components/release-source-evidence";
import { Unavailable } from "@/components/unavailable";
import { Badge } from "@/components/ui/badge";
import { getRelease, isNotFound } from "@/lib/api";

interface ReleasePageProps {
  params: Promise<{ id: string }>;
}

export async function generateMetadata({ params }: ReleasePageProps): Promise<Metadata> {
  const { id } = await params;
  try {
    const release = await getRelease(id);
    return {
      title: `${release.product.name} ${release.rawVersion}`,
      description: `Release ${release.rawVersion} of ${release.product.name}, with source and evidence.`,
      alternates: { canonical: `/releases/${release.id}` },
    };
  } catch {
    return { title: "Release" };
  }
}

export default async function ReleasePage({ params }: ReleasePageProps) {
  const { id } = await params;

  let release;
  try {
    release = await getRelease(id);
  } catch (error) {
    if (isNotFound(error)) notFound();
    return (
      <div className="mx-auto max-w-3xl px-4 py-12 sm:px-6">
        <h1 className="text-2xl font-bold tracking-tight text-ink-950">Release</h1>
        <div className="mt-6">
          <Unavailable />
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-3xl px-4 py-12 sm:px-6">
      <p className="text-sm text-ink-500">
        <Link href={`/products/${release.product.slug}`} className="hover:underline">
          {release.product.name}
        </Link>
      </p>
      <h1 className="mt-1 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-2xl font-bold tracking-tight text-ink-950">
        <span className="font-mono">{release.rawVersion}</span>
        <Badge variant="outline">{release.channel}</Badge>
        {release.withdrawn ? <Badge variant="danger">Withdrawn</Badge> : null}
        {release.recommended === true ? <Badge variant="success">Recommended</Badge> : null}
      </h1>

      <dl className="mt-6 grid grid-cols-1 gap-x-6 gap-y-4 text-sm sm:grid-cols-2">
        <div>
          <dt className="text-ink-500">Product</dt>
          <dd className="mt-0.5 text-ink-900">
            <Link href={`/products/${release.product.slug}`} className="hover:underline">
              {release.product.name}
            </Link>
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Release type</dt>
          <dd className="mt-0.5 text-ink-900">{release.releaseType}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Raw version</dt>
          <dd className="mt-0.5 font-mono text-ink-900">{release.rawVersion}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Normalized version</dt>
          <dd className="mt-0.5 font-mono text-ink-900">{release.normalizedVersion}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Release date</dt>
          <dd className="mt-0.5 text-ink-900">
            <ReleaseDate date={release.releaseDate} precision={release.releaseDatePrecision} />
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Recommended by vendor</dt>
          <dd className="mt-0.5 text-ink-900">
            {release.recommended === true
              ? "Yes"
              : release.recommended === false
                ? "Not recommended"
                : "The vendor has not designated a recommended version"}
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">First observed</dt>
          <dd className="mt-0.5 text-ink-900">
            {new Date(release.firstObservedAt).toISOString().replace("T", " ").slice(0, 16)} UTC
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Last verified</dt>
          <dd className="mt-0.5 text-ink-900">
            {new Date(release.lastVerifiedAt).toISOString().replace("T", " ").slice(0, 16)} UTC
          </dd>
        </div>
        {release.correctsReleaseId ? (
          <div>
            <dt className="text-ink-500">Corrects release</dt>
            <dd className="mt-0.5 text-ink-900">
              <Link href={`/releases/${release.correctsReleaseId}`} className="font-mono hover:underline">
                {release.correctsReleaseId}
              </Link>
            </dd>
          </div>
        ) : null}
      </dl>

      <section className="mt-10" aria-labelledby="evidence-heading">
        <h2 id="evidence-heading" className="text-lg font-semibold text-ink-950">
          Source and evidence
        </h2>
        <ReleaseSourceEvidence source={release.source} evidence={release.evidence} />
      </section>
    </div>
  );
}
