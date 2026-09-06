/**
 * Typed client for FirmScout's internal review-queue surface — reads only
 * (docs/architecture/api.md §11, "The internal review surface"; ADR-0021).
 *
 * This is not the public catalogue API: it lives under `/internal/`, is
 * off by default, and — per ADR-0021 — has no authentication at all. This
 * app used to also carry `decideReviewItem`, a server-side call to the
 * two write endpoints (`.../accept`, `.../reject`). It does not any more:
 * a public web app that runs a write to a private-network-only surface
 * server-side is itself an internet-reachable deputy for that surface,
 * which defeats the network placement ADR-0021 relies on as its only
 * control — a Server Action bound to `accept` executes wherever *this
 * app's* server runs, not wherever the operator believes the review API
 * is hidden. See ADR-0021 §"the public web app must not write" for the
 * incident this fixed and why the CLI (`firmscout review accept|reject`,
 * apps/cli/main.go) is the only decision path now: it requires a
 * database connection string and a shell on a host that has one, which a
 * public HTTP request can never forge.
 *
 * `reviewUiEnabled()` is this web app's own on/off switch, independent of
 * the API server's `Deps.ReviewAPIEnabled` (ADR-0021's "two independent
 * switches" principle applies here too): a deployment can build this app
 * with the review pages compiled in while still defaulting them to
 * invisible, and turning the API on does not by itself make this UI
 * appear. If the API switch is off but this one is on, the pages render
 * the same honest "temporarily unavailable" state any other unreachable
 * endpoint produces — there is no special-case detection of the other
 * switch's position, because nothing here can see it. Because only reads
 * remain, this switch now gates a read-only queue viewer, not a
 * moderation surface.
 *
 * Errors surface as the same `ApiError` lib/api.ts already defines, so a
 * caller catches one thing and renders one "problem" shape.
 */

import { ApiError, baseUrl, type Problem } from "./api";
import type { DatePrecision } from "./date";

// Independent of lib/api.ts's DEFAULT_TIMEOUT_MS on purpose: this is a
// different surface with its own contract, and a shared constant would
// make a future change to one silently change the other.
const DEFAULT_TIMEOUT_MS = 5000;

export type ReviewState = "open" | "in_progress" | "resolved" | "dismissed";
export type SlaClass = "urgent" | "high" | "standard" | "low";

export interface SubjectRef {
  type: string;
  id: string;
}

export interface VendorRef {
  slug: string;
  name: string;
}

export interface ProductRef {
  /** Absent when the read model knows a name but not a resolvable slug. */
  slug?: string;
  name: string;
}

/** Mirrors httpapi.ReviewItemDTO. */
export interface ReviewItem {
  id: string;
  kind: string;
  state: ReviewState;
  slaClass: SlaClass;
  priorityScore: number;
  title: string;
  detail?: string;
  subject: SubjectRef;
  vendor?: VendorRef;
  product?: ProductRef;
  ageSeconds: number;
  createdAt?: string;
  updatedAt?: string;
  resolvedAt?: string;
  resolvedBy?: string;
  resolution?: string;
}

export interface Pagination {
  nextCursor: string | null;
}

/** Mirrors httpapi.ReviewQueueResponse. */
export interface ReviewQueueResponse {
  items: ReviewItem[];
  pagination: Pagination;
}

/** Mirrors httpapi.GateResultDTO. */
export interface GateResult {
  gate: string;
  order: number;
  outcome: string;
  detail?: string;
  evaluatedBy: string;
  evaluatedAt?: string;
}

/**
 * Mirrors httpapi.ReviewCandidateDTO. releaseDate/releaseDatePrecision are
 * the same partial-date pair every other release-shaped object in this
 * app carries — feed them straight to components/release-date.tsx, never
 * through Date.parse.
 */
export interface ReviewCandidate {
  id: string;
  rawVersion: string;
  normalizedVersion: string;
  releaseType: string;
  channel?: string;
  releaseDate?: string;
  releaseDatePrecision: DatePrecision;
  confidence: number;
  state: string;
  productMatchHint?: string;
  releaseNotesUrl?: string;
}

/** Mirrors httpapi.ReviewSourceDTO. */
export interface ReviewSource {
  id: string;
  slug: string;
  url: string;
  official: boolean;
  qualityClass: string;
  health: string;
}

/** Mirrors httpapi.ConflictObservationDTO. */
export interface ConflictObservation {
  sourceId: string;
  qualityClass: string;
  official: boolean;
  eligible: boolean;
  rawVersion: string;
  releaseDate?: string;
  releaseDatePrecision: DatePrecision;
  observedAt?: string;
}

/** Mirrors httpapi.ConflictDTO. */
export interface Conflict {
  id: string;
  channel?: string;
  state: string;
  authorityRank: number;
  versions: string[];
  observations: ConflictObservation[];
  detectedAt?: string;
}

/** Mirrors httpapi.AuditEventDTO. */
export interface AuditEvent {
  actor: string;
  actorType: string;
  // Always false in this phase (ADR-0021). Rendered as-is, never hidden:
  // an asserted name must never be presented as a verified one.
  actorAuthenticated: boolean;
  action: string;
  reason?: string;
  occurredAt?: string;
}

/** Mirrors httpapi.Evidence (docs/api/openapi.yaml `Evidence` schema). */
export interface Evidence {
  retrievedAt: string;
  excerpt: string;
}

/**
 * Mirrors httpapi.ReviewItemDetailResponse. Each optional part carries its
 * own presence: `null` means "this part was genuinely missing" (a pruned
 * evidence row, a conflict already resolved between list and detail
 * requests), which is a different fact from an empty object and the page
 * must be able to tell them apart.
 */
export interface ReviewItemDetail {
  item: ReviewItem;
  candidate: ReviewCandidate | null;
  gates: GateResult[];
  evidence: Evidence | null;
  source: ReviewSource | null;
  conflict: Conflict | null;
  audit: AuditEvent[];
}

/** True when this deployment's build has switched the review UI on. */
export function reviewUiEnabled(): boolean {
  return process.env.FIRMSCOUT_REVIEW_UI_ENABLED === "true";
}

// ---------------------------------------------------------------------------
// Fetch wrapper
// ---------------------------------------------------------------------------

// GET-only on purpose: this module reads the queue and nothing else now (see the
// file header). There is no `method`, `headers` or `body` field here to reach for —
// that machinery was exactly what let a decision request travel from this app's
// server to the internal API, and removing it is what makes "this client cannot
// write" a fact about the code rather than a convention nobody enforces.
interface InternalRequestOptions {
  /** Single-valued query parameters; undefined values are omitted. */
  searchParams?: Record<string, string | number | undefined>;
  /**
   * Repeated query parameters (state/kind/sla on the queue list), each
   * appended once per value rather than joined, per api.md §11.1:
   * `?state=open&state=in_progress`, not `?state=open,in_progress`.
   */
  repeatedParams?: Record<string, readonly string[] | undefined>;
}

function buildInternalUrl(path: string, options: InternalRequestOptions): string {
  const url = new URL(`${baseUrl()}${path}`);
  for (const [key, value] of Object.entries(options.searchParams ?? {})) {
    if (value !== undefined) {
      url.searchParams.set(key, String(value));
    }
  }
  for (const [key, values] of Object.entries(options.repeatedParams ?? {})) {
    for (const value of values ?? []) {
      url.searchParams.append(key, value);
    }
  }
  return url.toString();
}

async function internalFetch<T>(path: string, options: InternalRequestOptions = {}): Promise<T> {
  const url = buildInternalUrl(path, options);

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), DEFAULT_TIMEOUT_MS);

  let response: Response;
  try {
    response = await fetch(url, {
      method: "GET",
      signal: controller.signal,
      headers: { Accept: "application/json" },
      // These routes are always Cache-Control: no-store on the server side
      // (api.md §11) and reflect a queue that changes under a reviewer's feet;
      // caching a page of it here would be the wrong kind of stale.
      cache: "no-store",
    });
  } catch (cause) {
    throw new ApiError(
      `FirmScout review API is unreachable at ${baseUrl()}: ${cause instanceof Error ? cause.message : String(cause)}`,
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
        // fall through to the generic "unexpected" case below.
      }
    }
    throw new ApiError(
      `FirmScout review API returned ${response.status} ${response.statusText}`,
      "unexpected",
      { status: response.status },
    );
  }

  return (await response.json()) as T;
}

// ---------------------------------------------------------------------------
// Endpoints (docs/architecture/api.md §11.1)
// ---------------------------------------------------------------------------

export function listReviewItems(params?: {
  state?: ReviewState[];
  kind?: string[];
  sla?: SlaClass[];
  vendor?: string;
  product?: string;
  limit?: number;
  cursor?: string;
}): Promise<ReviewQueueResponse> {
  return internalFetch<ReviewQueueResponse>("/internal/review/items", {
    searchParams: {
      vendor: params?.vendor,
      product: params?.product,
      limit: params?.limit,
      cursor: params?.cursor,
    },
    repeatedParams: {
      state: params?.state,
      kind: params?.kind,
      sla: params?.sla,
    },
  });
}

export function getReviewItem(id: string): Promise<ReviewItemDetail> {
  return internalFetch<ReviewItemDetail>(`/internal/review/items/${encodeURIComponent(id)}`);
}

// There is deliberately no decideReviewItem here any more. See the file header and
// ADR-0021: accepting or rejecting an item is a write to the catalogue, and this app
// must not be the thing that performs it — only the CLI (apps/cli/main.go, `review
// accept` / `review reject`) does, run by an operator who holds a database connection
// string directly.
