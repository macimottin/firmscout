/**
 * Typed client for the FirmScout API (docs/architecture/api.md,
 * docs/api/openapi.yaml).
 *
 * Every call happens server-side (Server Components, route handlers,
 * generateMetadata) — never in the browser — so no API key is ever bundled
 * into client JavaScript. Anonymous requests are permitted by the API
 * design (api.md §2) and are exactly what this client sends.
 *
 * The API is documented but, at the time this client was written, not
 * necessarily running. Every call can fail with a network error (DNS,
 * connection refused, timeout) in addition to the documented HTTP error
 * responses. Callers are expected to catch `ApiError` and render a calm
 * "temporarily unavailable" state rather than let the error propagate and
 * crash the page — see `isUnreachable()` below.
 */

import type { DatePrecision } from "./date";

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 5000;

/**
 * Exported so lib/review.ts can address the same origin's `/internal/`
 * surface without duplicating the FIRMSCOUT_API_URL resolution rule — the
 * review queue and the public catalogue are the same deployment, just
 * different route prefixes on it.
 */
export function baseUrl(): string {
  const configured = process.env.FIRMSCOUT_API_URL?.trim();
  return (
    configured && configured.length > 0 ? configured : DEFAULT_BASE_URL
  ).replace(/\/+$/, "");
}

// ---------------------------------------------------------------------------
// Schema types (mirrors docs/api/openapi.yaml `components.schemas`)
// ---------------------------------------------------------------------------

export type Channel = "stable" | "long_term" | "testing" | "development";

export type SourceKind = "html" | "text" | "json" | "xml" | "pdf" | "rss_atom";

export interface VendorSummary {
  slug: string;
  name: string;
}

export interface ProductSummary {
  slug: string;
  name: string;
}

/**
 * The product family shown on a product page: a name only, never a slug.
 * `ProductRef.Slug` is `omitempty` on the backend for this one field specifically
 * (docs: "the product family on a product summary knows a name but not a slug") --
 * there is no family route to link to, a family slug is unique per vendor rather
 * than globally, and it could not be served at `/products/{slug}` even in
 * principle. Kept distinct from `ProductSummary` (used for `vendor` and `runs`,
 * where a real, resolvable slug is always sent) so the type itself states the
 * guarantee rather than a comment next to an unused field.
 */
export interface FamilyRef {
  name: string;
}

export type ApplicabilityBasis =
  "own_releases" | "runs_os_unverified" | "none_recorded";

/**
 * Whether the releases this response carries are this product's own, and how many
 * there are (`OwnReleases` in `docs/api/openapi.yaml`). Always present inside
 * `firmwareApplicability`.
 *
 * `mapped` is deliberately not called `verified`. Its evidence is a
 * release-to-product mapping row, not a verification of which image a model takes
 * -- and confusing the two is the exact defect this member was added to fix.
 */
export interface OwnReleases {
  mapped: boolean;
  releaseCount: number;
}

/**
 * Whether the releases a product's page shows are known to apply to it
 * (ADR-0024). Always present on `Product`, never optional: an absent value would
 * read as "they apply", and "we have not verified which firmware image this model
 * takes" is a different claim from "every release of its operating system
 * applies" -- the same reasoning `hasSourceConflict` already rests on below.
 *
 * It answers TWO independent questions and keeps them apart, which is why
 * `ownReleases` sits beside `verified`/`basis` rather than inside `basis` as a
 * fourth member. `verified` and `basis` answer "which of ANOTHER product's
 * releases apply to this exact model"; `ownReleases` answers "are the releases in
 * this response this product's own". A product can need both answered at once -- a
 * rack server is a device AND publishes its own BIOS versions -- and while the two
 * were one scalar, such a product reported `{verified: true, basis: "own_releases"}`
 * and dropped the `runs_os` caveat entirely. See `docs/architecture/api.md` §3.2.
 */
export interface FirmwareApplicability {
  verified: boolean;
  /**
   * Deliberately widened to allow an unrecognised value: the API's vocabulary may
   * grow before this client does, and an unknown basis must render the cautious
   * fallback (lib/applicability.ts) rather than crash a page.
   */
  basis: ApplicabilityBasis | (string & {});
  /**
   * Required and always sent (`Applicability.required` in openapi.yaml). Read this,
   * never `basis`, to ask "does this product have releases of its own": since the
   * `runs` edge is now tested first, `basis === "own_releases"` means "no other
   * product's releases are in play", which is a different question.
   */
  ownReleases: OwnReleases;
}

export interface Vendor {
  slug: string;
  name: string;
  website: string;
  /**
   * Both are aggregate facts the vendor read model does not expose yet, so the API
   * omits them from every vendor response today. Typed optional because that is what
   * the wire actually carries -- typing them required is what turned the vendor page
   * into a 500.
   */
  productCount?: number;
  lastVerifiedAt?: string;
}

export interface OfficialSource {
  url: string;
  kind: SourceKind;
  official: boolean;
}

export interface ReleaseSummary {
  id: string;
  rawVersion: string;
  channel: Channel;
  /** Omitted entirely when releaseDatePrecision is "unknown". */
  releaseDate?: string;
  releaseDatePrecision: DatePrecision;
}

/**
 * Fields the MVP `Product` schema does not (yet) formally define, but
 * which the product page brief asks the UI to render when present:
 * aliases, EOL/EOS lifecycle status, and source quality. These are
 * intentionally optional and additive — the UI must render an honest
 * "not available" state when they are absent rather than assume the API
 * always supplies them (see blueprint §5: the free tier is limited by
 * convenience, never by inventing data).
 */
export interface EolStatus {
  status: "supported" | "eol" | "eos" | "unknown";
  eolDate?: string;
  eolDatePrecision?: DatePrecision;
  evidenceUrl?: string;
}

export type SourceQuality =
  "official_primary" | "official_secondary" | "community" | "unknown";

export interface Product {
  slug: string;
  name: string;
  vendor: VendorSummary;
  family: FamilyRef | null;
  /** Always present; null for a product that is not a hardware model. */
  modelIdentifier: string | null;
  /** Always present; empty for a product that runs nothing FirmScout catalogues. */
  runs: ProductSummary[];
  /** Always present. See lib/applicability.ts for the rendered sentence. */
  firmwareApplicability: FirmwareApplicability;
  /**
   * The product's primary category slug. Optional, and correctly so: the DTO field
   * is `omitempty` and a product in no category omits the key entirely -- which
   * `docs/api/openapi.yaml` now states by leaving `category` out of `Product.required`.
   * Typing it required is the same mistake `releaseType` below documents, one step
   * from rendering the literal string "undefined" in a definition list.
   */
  category?: string;
  /**
   * Absent for a hardware model, which publishes no release stream of its own: the
   * releases are published against the operating system it runs (ADR-0024). Typed
   * optional because the API genuinely omits the key -- typing it required is what
   * let a device page compile and then 500 at render.
   */
  releaseType?: string;
  latestRelease: ReleaseSummary | null;
  officialSources: OfficialSource[];
  /** Absent until some source has verified this product; never true of a device. */
  lastVerifiedAt?: string;
  /**
   * The other strings this product is known by -- marketing names, keyboard-typeable
   * spellings, and above all the vendor's model number, which is the string a fleet
   * inventory actually holds.
   *
   * Always present and never null: an empty array when the catalogue holds none.
   * `Product.required` in `docs/api/openapi.yaml` guarantees it, and the presenter
   * builds a non-nil slice for every product. It is on the wire because the alias is
   * frequently the reason the caller is on this page at all -- ADR-0024 makes
   * model-number search work through a `model_number` alias, so the hit that brought
   * a fleet manager here was produced by a string the response used not to send back.
   */
  aliases: string[];
  // Forward-looking, not guaranteed by the current documented contract:
  eol?: EolStatus;
  sourceQuality?: SourceQuality;
  // Always present (Phase 2, D25 / ADR-0020): a disagreement between eligible
  // sources is a finding the platform surfaces, never one it silently
  // resolves, so the UI must never treat an absent key as "no conflict".
  hasSourceConflict: boolean;
  /**
   * Detail behind `hasSourceConflict`, additive alongside it -- confirmed against the
   * real backend response and openapi.yaml's `Conflict` schema (`docs/api/openapi.yaml`,
   * `docs/architecture/api.md` §3.2). No `?`, matching `family`/`latestRelease` above:
   * the key is always present, never omitted, the same guarantee `hasSourceConflict`
   * carries. It is `null` both when there is no open conflict and, honestly rather than
   * defensively, when one is open but nothing has recorded a channel and versions for it
   * yet -- a product summary computed before this field existed, or a conflict the
   * detector opened without finishing that detail. Either way the UI falls back to the
   * same generic banner text (see lib/conflict.ts) rather than crash or show "undefined".
   */
  conflict: SourceConflictDetail | null;
}

/**
 * What is actually in dispute behind a `hasSourceConflict: true`, mirroring
 * `domain.SourceConflict` (Channel, Versions, SourceIDs, DetectedAt) minus per-source
 * identity, which the API does not expose here -- render only what is actually sent,
 * never invent a source name or URL to fill the gap.
 */
export interface SourceConflictDetail {
  /**
   * Empty when the disputing sources stated no channel -- a real, valid case
   * (`domain.SourceObservation` allows an unset channel), not a narrower enum than the
   * backend actually sends. Deliberately `string`, not `Channel`: the `Channel` union
   * has no empty member, and narrowing this to it would make a legitimately empty
   * value a type error instead of a value `lib/conflict.ts` already renders correctly.
   */
  channel: string;
  versions: string[];
  sourceCount: number;
  detectedAt: string;
}

export interface Evidence {
  retrievedAt: string;
  excerpt: string;
}

export interface ReleaseSource {
  official: boolean;
  url: string;
  kind: SourceKind;
}

export interface Release {
  id: string;
  product: ProductSummary;
  rawVersion: string;
  normalizedVersion: string;
  releaseType: string;
  channel: Channel;
  /** Omitted entirely when releaseDatePrecision is "unknown". */
  releaseDate?: string;
  releaseDatePrecision: DatePrecision;
  /** null unless the vendor explicitly designated this version recommended. */
  recommended: boolean | null;
  withdrawn: boolean;
  correctsReleaseId: string | null;
  source: ReleaseSource;
  evidence: Evidence;
  firstObservedAt: string;
  lastVerifiedAt: string;
}

export interface Pagination {
  nextCursor: string | null;
}

export interface VendorListResponse {
  vendors: Vendor[];
  pagination: Pagination;
}

/**
 * Tells a caller whether the page they are reading is the product's whole
 * archive or a plan-bounded slice of it (Phase 2, D22/D23). A consumer
 * that cannot distinguish a windowed history from a complete one has been
 * misled by omission, so this is always present, never inferred from
 * response length.
 */
export interface HistoryWindow {
  windowed: boolean;
  since?: string;
  detail?: string;
}

export interface ReleaseListResponse {
  releases: Release[];
  pagination: Pagination;
  window: HistoryWindow;
}

export interface LatestReleaseResponse {
  vendor: VendorSummary;
  product: ProductSummary;
  latestRelease: Release;
  /**
   * Null when the vendor never designated a recommended build. It is a separate member
   * rather than a flag on latestRelease because the two can be different releases: the
   * newest version is not automatically the safest, and a vendor recommending an older
   * build is making a statement this client passes on rather than overrides.
   */
  recommendedRelease: Release | null;
  /**
   * Whether eligible sources disagree about this product's newest version. Always
   * present, never optional: an absent key would read as "no conflict", and "we checked
   * and they agree" is a different claim from "we did not check" (ADR-0020).
   */
  hasSourceConflict: boolean;
  /** Same field, same caveats, as `Product.conflict` above. */
  conflict: SourceConflictDetail | null;
}

export type SearchResultType = "product" | "vendor";
export type SearchMatchedOn = "name" | "alias" | "vendor";

export interface SearchResultItem {
  type: SearchResultType;
  slug: string;
  name: string;
  /**
   * The vendor's product code, echoed back on a hit so a fleet manager who
   * pasted the string stamped on the chassis can see this row is the thing
   * they hold. Omitted (not null) for a result that is not a hardware model
   * or is not a product at all -- a compact hit descriptor, unlike the detail
   * response's always-present field (api.md §3.2).
   */
  modelIdentifier?: string;
  vendor: VendorSummary | null;
  matchedOn: SearchMatchedOn;
}

export interface SearchResponse {
  results: SearchResultItem[];
  pagination: Pagination;
}

/** RFC 9457 application/problem+json. */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  requestId?: string;
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

export type ApiErrorKind =
  | "network" // fetch failed, timed out, or DNS/connection error: the API is unreachable
  | "problem" // a well-formed RFC 9457 problem+json response
  | "unexpected"; // a non-2xx response that wasn't valid problem+json

export class ApiError extends Error {
  readonly kind: ApiErrorKind;
  readonly status?: number;
  readonly problem?: Problem;

  constructor(
    message: string,
    kind: ApiErrorKind,
    options?: { status?: number; problem?: Problem; cause?: unknown },
  ) {
    super(message, { cause: options?.cause });
    this.name = "ApiError";
    this.kind = kind;
    this.status = options?.status;
    this.problem = options?.problem;
  }

  get isNotFound(): boolean {
    return this.status === 404;
  }
}

/** True when the API could not be reached at all (down, DNS, timeout, refused). */
export function isUnreachable(error: unknown): boolean {
  return error instanceof ApiError && error.kind === "network";
}

/** True when the API responded with a well-formed 404 problem. */
export function isNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.isNotFound;
}

// ---------------------------------------------------------------------------
// Fetch wrapper
// ---------------------------------------------------------------------------

interface RequestOptions {
  /** Query string parameters; undefined values are omitted. */
  searchParams?: Record<string, string | number | undefined>;
  /** Next.js ISR revalidation window in seconds. */
  revalidate?: number | false;
  timeoutMs?: number;
}

function buildUrl(
  path: string,
  searchParams?: RequestOptions["searchParams"],
): string {
  const url = new URL(`${baseUrl()}/api/v1${path}`);
  if (searchParams) {
    for (const [key, value] of Object.entries(searchParams)) {
      if (value !== undefined) {
        url.searchParams.set(key, String(value));
      }
    }
  }
  return url.toString();
}

async function apiFetch<T>(
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const url = buildUrl(path, options.searchParams);
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);

  let response: Response;
  try {
    response = await fetch(url, {
      signal: controller.signal,
      headers: { Accept: "application/json" },
      next:
        options.revalidate === false
          ? undefined
          : { revalidate: options.revalidate ?? 60 },
      cache: options.revalidate === false ? "no-store" : undefined,
    });
  } catch (cause) {
    throw new ApiError(
      `FirmScout API is unreachable at ${baseUrl()}: ${cause instanceof Error ? cause.message : String(cause)}`,
      "network",
      { cause },
    );
  } finally {
    clearTimeout(timeout);
  }

  if (!response.ok) {
    const contentType = response.headers.get("content-type") ?? "";
    if (
      contentType.includes("application/problem+json") ||
      contentType.includes("application/json")
    ) {
      try {
        const problem = (await response.json()) as Problem;
        throw new ApiError(problem.detail ?? problem.title, "problem", {
          status: response.status,
          problem,
        });
      } catch (err) {
        if (err instanceof ApiError) throw err;
        // fall through to unexpected
      }
    }
    throw new ApiError(
      `FirmScout API returned ${response.status} ${response.statusText}`,
      "unexpected",
      {
        status: response.status,
      },
    );
  }

  return (await response.json()) as T;
}

// ---------------------------------------------------------------------------
// Endpoints (docs/architecture/api.md §2)
// ---------------------------------------------------------------------------

export function search(
  q: string,
  options?: { limit?: number; cursor?: string },
): Promise<SearchResponse> {
  return apiFetch<SearchResponse>("/search", {
    searchParams: { q, limit: options?.limit, cursor: options?.cursor },
    revalidate: 60,
  });
}

export function listVendors(options?: {
  limit?: number;
  cursor?: string;
}): Promise<VendorListResponse> {
  return apiFetch<VendorListResponse>("/vendors", {
    searchParams: { limit: options?.limit, cursor: options?.cursor },
    revalidate: 3600,
  });
}

export function getVendor(slug: string): Promise<Vendor> {
  return apiFetch<Vendor>(`/vendors/${encodeURIComponent(slug)}`, {
    revalidate: 3600,
  });
}

export function getProduct(slug: string): Promise<Product> {
  return apiFetch<Product>(`/products/${encodeURIComponent(slug)}`, {
    revalidate: 3600,
  });
}

export function listProductReleases(
  slug: string,
  options?: {
    channel?: Channel;
    releaseType?: string;
    sort?: "releaseDate" | "firstObservedAt";
    order?: "asc" | "desc";
    limit?: number;
    cursor?: string;
  },
): Promise<ReleaseListResponse> {
  return apiFetch<ReleaseListResponse>(
    `/products/${encodeURIComponent(slug)}/releases`,
    {
      searchParams: {
        channel: options?.channel,
        releaseType: options?.releaseType,
        sort: options?.sort,
        order: options?.order,
        limit: options?.limit,
        cursor: options?.cursor,
      },
      revalidate: 300,
    },
  );
}

export function getLatestRelease(
  slug: string,
  options?: { channel?: Channel },
): Promise<LatestReleaseResponse> {
  return apiFetch<LatestReleaseResponse>(
    `/products/${encodeURIComponent(slug)}/latest`,
    {
      searchParams: { channel: options?.channel },
      revalidate: 300,
    },
  );
}

export function getRelease(id: string): Promise<Release> {
  return apiFetch<Release>(`/releases/${encodeURIComponent(id)}`, {
    revalidate: false,
  });
}

export interface HealthStatus {
  status: "ok" | "degraded" | "unavailable";
}

/**
 * `GET /healthz` lives outside the versioned `/api/v1` prefix
 * (docs/architecture/api.md §2), so it bypasses `apiFetch`'s URL builder.
 * Used by the status page; never throws for a reachability failure — it
 * reports "unavailable" instead, since that IS the status being reported.
 */
export async function checkHealth(): Promise<HealthStatus> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), DEFAULT_TIMEOUT_MS);
  try {
    const response = await fetch(`${baseUrl()}/healthz`, {
      signal: controller.signal,
      headers: { Accept: "application/json" },
      cache: "no-store",
    });
    if (!response.ok && response.status !== 503) {
      return { status: "unavailable" };
    }
    const body = (await response.json()) as HealthStatus;
    return body;
  } catch {
    return { status: "unavailable" };
  } finally {
    clearTimeout(timeout);
  }
}
