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

function baseUrl(): string {
  const configured = process.env.FIRMSCOUT_API_URL?.trim();
  return (configured && configured.length > 0 ? configured : DEFAULT_BASE_URL).replace(
    /\/+$/,
    "",
  );
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

export interface Vendor {
  slug: string;
  name: string;
  website: string;
  productCount: number;
  lastVerifiedAt: string;
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
 * aliases, EOL/EOS lifecycle status, source quality, and a conflicting-
 * source flag. These are intentionally optional and additive — the UI
 * must render an honest "not available" state when they are absent
 * rather than assume the API always supplies them (see blueprint §5:
 * the free tier is limited by convenience, never by inventing data).
 */
export interface EolStatus {
  status: "supported" | "eol" | "eos" | "unknown";
  eolDate?: string;
  eolDatePrecision?: DatePrecision;
  evidenceUrl?: string;
}

export type SourceQuality = "official_primary" | "official_secondary" | "community" | "unknown";

export interface Product {
  slug: string;
  name: string;
  vendor: VendorSummary;
  family: ProductSummary | null;
  category: string;
  releaseType: string;
  latestRelease: ReleaseSummary | null;
  officialSources: OfficialSource[];
  lastVerifiedAt: string;
  // Forward-looking, not guaranteed by the current documented contract:
  aliases?: string[];
  eol?: EolStatus;
  sourceQuality?: SourceQuality;
  hasSourceConflict?: boolean;
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

export interface ReleaseListResponse {
  releases: Release[];
  pagination: Pagination;
}

export interface LatestReleaseResponse {
  vendor: VendorSummary;
  product: ProductSummary;
  latestRelease: Release;
}

export type SearchResultType = "product" | "vendor";
export type SearchMatchedOn = "name" | "alias" | "vendor";

export interface SearchResultItem {
  type: SearchResultType;
  slug: string;
  name: string;
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

  constructor(message: string, kind: ApiErrorKind, options?: { status?: number; problem?: Problem; cause?: unknown }) {
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

function buildUrl(path: string, searchParams?: RequestOptions["searchParams"]): string {
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

async function apiFetch<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const url = buildUrl(path, options.searchParams);
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);

  let response: Response;
  try {
    response = await fetch(url, {
      signal: controller.signal,
      headers: { Accept: "application/json" },
      next: options.revalidate === false ? undefined : { revalidate: options.revalidate ?? 60 },
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
    if (contentType.includes("application/problem+json") || contentType.includes("application/json")) {
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
    throw new ApiError(`FirmScout API returned ${response.status} ${response.statusText}`, "unexpected", {
      status: response.status,
    });
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

export function listVendors(options?: { limit?: number; cursor?: string }): Promise<VendorListResponse> {
  return apiFetch<VendorListResponse>("/vendors", {
    searchParams: { limit: options?.limit, cursor: options?.cursor },
    revalidate: 3600,
  });
}

export function getVendor(slug: string): Promise<Vendor> {
  return apiFetch<Vendor>(`/vendors/${encodeURIComponent(slug)}`, { revalidate: 3600 });
}

export function getProduct(slug: string): Promise<Product> {
  return apiFetch<Product>(`/products/${encodeURIComponent(slug)}`, { revalidate: 3600 });
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
  return apiFetch<ReleaseListResponse>(`/products/${encodeURIComponent(slug)}/releases`, {
    searchParams: {
      channel: options?.channel,
      releaseType: options?.releaseType,
      sort: options?.sort,
      order: options?.order,
      limit: options?.limit,
      cursor: options?.cursor,
    },
    revalidate: 300,
  });
}

export function getLatestRelease(
  slug: string,
  options?: { channel?: Channel },
): Promise<LatestReleaseResponse> {
  return apiFetch<LatestReleaseResponse>(`/products/${encodeURIComponent(slug)}/latest`, {
    searchParams: { channel: options?.channel },
    revalidate: 300,
  });
}

export function getRelease(id: string): Promise<Release> {
  return apiFetch<Release>(`/releases/${encodeURIComponent(id)}`, { revalidate: false });
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
