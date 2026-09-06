/**
 * Turns a `SourceConflictDetail` (api.ts) into the two strings the "Conflicting
 * sources" banner shows. Kept as a pure function, tested directly (conflict.test.ts),
 * because the fallback path -- an old cached summary, or a conflict flagged before its
 * detail was captured -- is exactly the case that must never crash or print
 * "undefined", and that is easiest to prove true of a function than of a rendered page.
 */

import type { SourceConflictDetail } from "./api";

export interface ConflictBanner {
  title: string;
  description: string;
}

const GENERIC_DESCRIPTION =
  "Two or more official sources disagree about this product's current facts. " +
  "FirmScout does not auto-resolve source conflicts — the values below are shown as " +
  "published, and a human review is pending.";

/**
 * Falls back to today's generic banner text whenever detail is absent, null, or too
 * malformed to describe honestly (an empty version list or a non-positive source
 * count) -- rather than render a half-sentence, since a vague truth beats a precise
 * guess but an outright fabrication is not on the table either way.
 */
export function describeConflict(detail: SourceConflictDetail | null | undefined): ConflictBanner {
  if (!detail || detail.versions.length === 0 || detail.sourceCount <= 0) {
    return { title: "Conflicting sources", description: GENERIC_DESCRIPTION };
  }

  const sourceWord = detail.sourceCount === 1 ? "source" : "sources";
  const versionList = detail.versions.join(", ");
  const detectedOn = formatDetectedAt(detail.detectedAt);
  // channel is real but legitimately empty when the disputing sources stated none
  // (domain.SourceObservation) -- rendered as no channel clause at all rather than
  // "on the  channel", which would read as a rendering bug rather than an honest gap.
  const channelClause = detail.channel ? ` on the ${detail.channel} channel` : "";

  return {
    title: `Conflicting sources${channelClause}`,
    description:
      `${detail.sourceCount} eligible ${sourceWord} report different current versions` +
      `${channelClause}: reported versions are ${versionList}. FirmScout does ` +
      `not auto-resolve source conflicts — the values below are shown as published, and a ` +
      `human review is pending${detectedOn ? ` (detected ${detectedOn})` : ""}.`,
  };
}

/** Returns a bare ISO date, or null for a missing/unparseable timestamp -- never a guess. */
function formatDetectedAt(detectedAt: string): string | null {
  if (!detectedAt) return null;
  const parsed = new Date(detectedAt);
  return Number.isNaN(parsed.getTime()) ? null : parsed.toISOString().slice(0, 10);
}
