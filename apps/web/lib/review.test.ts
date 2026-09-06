import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { formatReleaseDate } from "./date";
import { getReviewItem, listReviewItems, reviewUiEnabled } from "./review";

const originalFetch = global.fetch;
const originalEnv = { ...process.env };

beforeEach(() => {
  process.env = { ...originalEnv };
});

afterEach(() => {
  global.fetch = originalFetch;
  vi.restoreAllMocks();
  process.env = { ...originalEnv };
});

function jsonResponse(body: unknown, init?: { status?: number; contentType?: string }) {
  return new Response(JSON.stringify(body), {
    status: init?.status ?? 200,
    headers: { "content-type": init?.contentType ?? "application/json" },
  });
}

describe("reviewUiEnabled", () => {
  it("is false when the env var is unset", () => {
    delete process.env.FIRMSCOUT_REVIEW_UI_ENABLED;
    expect(reviewUiEnabled()).toBe(false);
  });

  it("is false for any value other than the literal string \"true\"", () => {
    process.env.FIRMSCOUT_REVIEW_UI_ENABLED = "1";
    expect(reviewUiEnabled()).toBe(false);
  });

  it("is true only when explicitly set to \"true\"", () => {
    process.env.FIRMSCOUT_REVIEW_UI_ENABLED = "true";
    expect(reviewUiEnabled()).toBe(true);
  });
});

// There is deliberately no describe("decideReviewItem", ...) here any more: the
// function it tested was removed from lib/review.ts along with the rest of this app's
// write path (see that file's header and ADR-0021). This module is reads only now, and
// its test suite covers exactly that.

describe("listReviewItems", () => {
  it("serialises repeated state/kind/sla parameters as repeated query keys, not a joined list", async () => {
    const fetchSpy = vi
      .fn()
      .mockResolvedValue(jsonResponse({ items: [], pagination: { nextCursor: null } }));
    global.fetch = fetchSpy as unknown as typeof fetch;

    await listReviewItems({
      state: ["open", "in_progress"],
      kind: ["multi_source_conflict"],
      sla: ["urgent", "high"],
      vendor: "fortinet",
      limit: 25,
    });

    const [url] = fetchSpy.mock.calls[0] as [string];
    const parsed = new URL(url);
    expect(parsed.searchParams.getAll("state")).toEqual(["open", "in_progress"]);
    expect(parsed.searchParams.getAll("kind")).toEqual(["multi_source_conflict"]);
    expect(parsed.searchParams.getAll("sla")).toEqual(["urgent", "high"]);
    expect(parsed.searchParams.get("vendor")).toBe("fortinet");
    expect(parsed.searchParams.get("limit")).toBe("25");
    // The GET verb, and no body — this is a read.
    const [, init] = fetchSpy.mock.calls[0] as [string, RequestInit];
    expect(init.method ?? "GET").toBe("GET");
  });

  it("omits parameters that were not supplied rather than sending empty keys", async () => {
    const fetchSpy = vi
      .fn()
      .mockResolvedValue(jsonResponse({ items: [], pagination: { nextCursor: null } }));
    global.fetch = fetchSpy as unknown as typeof fetch;

    await listReviewItems();

    const [url] = fetchSpy.mock.calls[0] as [string];
    const parsed = new URL(url);
    expect([...parsed.searchParams.keys()]).toEqual([]);
  });

  it("throws a network ApiError when the review API is unreachable", async () => {
    global.fetch = vi.fn().mockRejectedValue(new TypeError("fetch failed")) as unknown as typeof fetch;

    await expect(listReviewItems()).rejects.toMatchObject({ kind: "network" });
  });
});

describe("getReviewItem — date-precision rendering for a review item", () => {
  it("passes a month-only candidate release date through unchanged, so it never renders a day", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      jsonResponse({
        item: {
          id: "rev_abc123",
          kind: "multi_source_conflict",
          state: "open",
          slaClass: "high",
          priorityScore: 210,
          title: "Two sources disagree",
          subject: { type: "candidate_release", id: "cand_1" },
          ageSeconds: 3600,
        },
        candidate: {
          id: "cand_1",
          rawVersion: "7.24",
          normalizedVersion: "7.24",
          releaseType: "firmware",
          releaseDate: "2026-02",
          releaseDatePrecision: "month_only",
          confidence: 0.72,
          state: "human_review_required",
        },
        gates: [],
        evidence: null,
        source: null,
        conflict: {
          id: "conf_1",
          state: "open",
          authorityRank: 40,
          versions: ["7.24", "7.24.1"],
          observations: [
            {
              sourceId: "src_a",
              qualityClass: "official_manufacturer",
              official: true,
              eligible: true,
              rawVersion: "7.24.1",
              releaseDate: "2026",
              releaseDatePrecision: "year_only",
            },
          ],
        },
        audit: [],
      }),
    ) as unknown as typeof fetch;

    const detail = await getReviewItem("rev_abc123");

    // The client must not run a partial date through Date.parse or any
    // component that would fill in a day/month — it hands the string and
    // its precision through exactly as the API sent them.
    expect(detail.candidate?.releaseDate).toBe("2026-02");
    expect(detail.candidate?.releaseDatePrecision).toBe("month_only");
    expect(detail.conflict?.observations[0]?.releaseDate).toBe("2026");
    expect(detail.conflict?.observations[0]?.releaseDatePrecision).toBe("year_only");

    // And when that untouched pair reaches the same formatter
    // components/release-date.tsx uses, it renders with no day — the
    // defining product rule (README "No invented dates") holding all the
    // way from this client to the page.
    expect(formatReleaseDate(detail.candidate?.releaseDate, detail.candidate?.releaseDatePrecision ?? "unknown")).toBe(
      "February 2026",
    );
    expect(
      formatReleaseDate(
        detail.conflict?.observations[0]?.releaseDate,
        detail.conflict?.observations[0]?.releaseDatePrecision ?? "unknown",
      ),
    ).toBe("2026");
  });

  it("surfaces a not-found problem+json response as an ApiError, not a thrown TypeError", async () => {
    const problem = {
      type: "https://firmscout.dev/problems/not-found",
      title: "Resource not found",
      status: 404,
      detail: "No review item matches this id.",
    };
    global.fetch = vi
      .fn()
      .mockResolvedValue(jsonResponse(problem, { status: 404, contentType: "application/problem+json" })) as unknown as typeof fetch;

    await expect(getReviewItem("rev_missing")).rejects.toMatchObject({
      kind: "problem",
      status: 404,
      problem,
    });
  });
});
