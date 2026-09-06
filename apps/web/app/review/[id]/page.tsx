import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { isNotFound, isUnreachable } from "@/lib/api";
import { getReviewItem, reviewUiEnabled } from "@/lib/review";
import { ReleaseDate } from "@/components/release-date";
import { Unavailable } from "@/components/unavailable";
import { UnauthenticatedBanner } from "@/components/review/unauthenticated-banner";
import { GateTable } from "@/components/review/gate-table";
import { ConflictPanel } from "@/components/review/conflict-panel";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";

export const metadata: Metadata = {
  title: "Review item",
  robots: { index: false, follow: false },
};

export const revalidate = 0;

interface ReviewItemPageProps {
  params: Promise<{ id: string }>;
}

export default async function ReviewItemPage({ params }: ReviewItemPageProps) {
  if (!reviewUiEnabled()) {
    notFound();
  }

  const { id } = await params;

  let detail;
  try {
    detail = await getReviewItem(id);
  } catch (error) {
    if (isNotFound(error)) notFound();
    return (
      <div className="mx-auto max-w-4xl px-4 py-12 sm:px-6">
        <UnauthenticatedBanner />
        <h1 className="text-2xl font-bold tracking-tight text-ink-950">Review item</h1>
        <div className="mt-6">
          {isUnreachable(error) ? (
            <Unavailable />
          ) : (
            <Unavailable
              title="Could not load this review item"
              detail="The review API returned something this page didn't expect. Please try again."
            />
          )}
        </div>
      </div>
    );
  }

  const { item, candidate, gates, evidence, source, conflict, audit } = detail;
  const decidable = item.state === "open" || item.state === "in_progress";

  return (
    <div className="mx-auto max-w-4xl px-4 py-12 sm:px-6">
      <UnauthenticatedBanner />

      <p className="text-sm text-ink-500">
        <Link href="/review" className="hover:underline">
          Review queue
        </Link>
      </p>
      <h1 className="mt-1 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-2xl font-bold tracking-tight text-ink-950">
        {item.title}
        <Badge variant="outline">{item.slaClass}</Badge>
        <Badge variant={item.state === "open" || item.state === "in_progress" ? "warning" : "default"}>
          {item.state.replace(/_/g, " ")}
        </Badge>
      </h1>
      {item.detail ? <p className="mt-2 text-sm text-ink-600">{item.detail}</p> : null}

      <dl className="mt-6 grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
        <div>
          <dt className="text-ink-500">Priority score</dt>
          <dd className="mt-0.5 text-ink-900">{item.priorityScore}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Kind</dt>
          <dd className="mt-0.5 text-ink-900">{item.kind.replace(/_/g, " ")}</dd>
        </div>
        <div>
          <dt className="text-ink-500">Vendor</dt>
          <dd className="mt-0.5 text-ink-900">
            {item.vendor ? (
              <Link href={`/vendors/${item.vendor.slug}`} className="hover:underline">
                {item.vendor.name}
              </Link>
            ) : (
              "—"
            )}
          </dd>
        </div>
        <div>
          <dt className="text-ink-500">Product</dt>
          <dd className="mt-0.5 text-ink-900">
            {item.product?.slug ? (
              <Link href={`/products/${item.product.slug}`} className="hover:underline">
                {item.product.name}
              </Link>
            ) : (
              item.product?.name ?? "—"
            )}
          </dd>
        </div>
        {!decidable ? (
          <>
            <div>
              <dt className="text-ink-500">Resolution</dt>
              <dd className="mt-0.5 text-ink-900">{item.resolution ?? "—"}</dd>
            </div>
            <div>
              <dt className="text-ink-500">Resolved by</dt>
              <dd className="mt-0.5 text-ink-900">{item.resolvedBy ?? "—"}</dd>
            </div>
          </>
        ) : null}
      </dl>

      <section className="mt-10" aria-labelledby="candidate-heading">
        <h2 id="candidate-heading" className="text-lg font-semibold text-ink-950">
          Candidate version
        </h2>
        <div className="mt-4">
          {candidate ? (
            <div className="rounded-lg border border-ink-200 bg-white p-5 text-sm">
              <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                <span className="font-mono text-lg font-semibold text-ink-950">
                  {candidate.rawVersion}
                </span>
                {candidate.channel ? <Badge variant="outline">{candidate.channel}</Badge> : null}
                <Badge variant="outline">{candidate.releaseType.replace(/_/g, " ")}</Badge>
              </div>
              <dl className="mt-4 grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
                <div>
                  <dt className="text-ink-500">Normalized version</dt>
                  <dd className="mt-0.5 font-mono text-ink-900">{candidate.normalizedVersion}</dd>
                </div>
                <div>
                  <dt className="text-ink-500">Release date</dt>
                  <dd className="mt-0.5 text-ink-900">
                    <ReleaseDate date={candidate.releaseDate} precision={candidate.releaseDatePrecision} />
                  </dd>
                </div>
                <div>
                  <dt className="text-ink-500">Confidence</dt>
                  <dd className="mt-0.5 text-ink-900">{candidate.confidence.toFixed(2)}</dd>
                </div>
                <div>
                  <dt className="text-ink-500">Candidate state</dt>
                  <dd className="mt-0.5 text-ink-900">{candidate.state.replace(/_/g, " ")}</dd>
                </div>
              </dl>
              {candidate.releaseNotesUrl ? (
                <p className="mt-4 text-ink-600">
                  <a
                    href={candidate.releaseNotesUrl}
                    rel="noopener noreferrer"
                    target="_blank"
                    className="text-ink-800 underline-offset-2 hover:underline"
                  >
                    Release notes
                  </a>
                </p>
              ) : null}
            </div>
          ) : (
            <p className="text-sm text-ink-500">
              The candidate behind this item is no longer on record.
            </p>
          )}
        </div>
      </section>

      {conflict ? (
        <section className="mt-10" aria-labelledby="conflict-heading">
          <h2 id="conflict-heading" className="text-lg font-semibold text-ink-950">
            Competing versions
          </h2>
          <div className="mt-4">
            <ConflictPanel conflict={conflict} />
          </div>
        </section>
      ) : null}

      <section className="mt-10" aria-labelledby="evidence-heading">
        <h2 id="evidence-heading" className="text-lg font-semibold text-ink-950">
          Evidence
        </h2>
        <div className="mt-4">
          {evidence ? (
            <div className="rounded-lg border border-ink-200 bg-white p-5 text-sm">
              {source ? (
                <p className="text-ink-600">
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
                  ) : (
                    <Badge variant="outline" className="ml-2">
                      Unofficial
                    </Badge>
                  )}
                </p>
              ) : (
                <p className="text-ink-500">The source this evidence came from is no longer on record.</p>
              )}
              <p className="mt-3 text-ink-500">
                Retrieved {new Date(evidence.retrievedAt).toISOString().replace("T", " ").slice(0, 16)} UTC
              </p>
              <p className="mt-2 rounded bg-ink-50 p-3 font-mono text-xs text-ink-700">
                &ldquo;{evidence.excerpt}&rdquo;
              </p>
            </div>
          ) : (
            <p className="text-sm text-ink-500">No evidence row is on record for this candidate.</p>
          )}
        </div>
      </section>

      <section className="mt-10" aria-labelledby="gates-heading">
        <h2 id="gates-heading" className="text-lg font-semibold text-ink-950">
          Validation gates
        </h2>
        <div className="mt-4">
          <GateTable gates={gates} />
        </div>
      </section>

      {audit.length > 0 ? (
        <section className="mt-10" aria-labelledby="audit-heading">
          <h2 id="audit-heading" className="text-lg font-semibold text-ink-950">
            Audit trail
          </h2>
          <ul className="mt-4 space-y-3 text-sm">
            {audit.map((event, i) => (
              <li key={i} className="rounded-lg border border-ink-200 bg-white p-4">
                <p className="text-ink-900">
                  <span className="font-medium">{event.actor}</span>{" "}
                  <span className="text-ink-500">
                    ({event.actorType}
                    {!event.actorAuthenticated ? ", unverified" : ""})
                  </span>{" "}
                  {event.action.replace(/\./g, " ")}
                  {event.occurredAt ? (
                    <span className="text-ink-500">
                      {" "}
                      · {new Date(event.occurredAt).toISOString().replace("T", " ").slice(0, 16)} UTC
                    </span>
                  ) : null}
                </p>
                {event.reason ? <p className="mt-1 text-ink-600">{event.reason}</p> : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {decidable ? (
        <section className="mt-10" aria-labelledby="decision-heading">
          <h2 id="decision-heading" className="text-lg font-semibold text-ink-950">
            Decide
          </h2>
          <div className="mt-4">
            {/*
              There is deliberately no accept/reject form here any more. This app used
              to run those as Server Actions against the internal API, server-side —
              which made this public site an internet-reachable deputy for an
              unauthenticated write surface that is only supposed to be reachable from
              a private network (ADR-0021). The fix is not a warning; it is that this
              page cannot perform the write at all. Deciding this item now happens only
              from the CLI, run by an operator who holds a database connection string.
            */}
            <Alert variant="warning">
              <AlertTitle>Decide from the CLI, not from this page</AlertTitle>
              <AlertDescription>
                This web app cannot accept or reject a candidate — that write happens only
                through the operator CLI, run against the database directly (ADR-0021):
                <code className="mt-2 block overflow-x-auto rounded bg-ink-900/90 px-3 py-2 font-mono text-xs text-ink-50">
                  firmscout review accept --id {item.id} --actor &quot;your name&quot; --reason
                  &quot;...&quot;
                </code>
                <code className="mt-2 block overflow-x-auto rounded bg-ink-900/90 px-3 py-2 font-mono text-xs text-ink-50">
                  firmscout review reject --id {item.id} --actor &quot;your name&quot; --reason
                  &quot;...&quot;
                </code>
              </AlertDescription>
            </Alert>
          </div>
        </section>
      ) : null}
    </div>
  );
}
