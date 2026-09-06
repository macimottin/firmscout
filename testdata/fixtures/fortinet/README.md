# Fortinet collector fixtures

Every file in this directory is **test data**. Nothing here is seeded into a
database, published through the API, or presented anywhere as a statement about
what Fortinet has published. This fixture is not a recording, and no test in
this repository fetches a vendor site.

## `psirt-advisories.fixture.xml` — synthetic-to-measured-shape

| | |
| --- | --- |
| Source URL (published) | `https://www.fortiguard.com/rss/ir.xml` |
| Source URL (registered, redirect target) | `https://filestore.fortinet.com/fortiguard/rss/ir.xml` |
| Measured | 2026-09-05 (UTC) |
| Original response | `GET https://www.fortiguard.com/rss/ir.xml` → 302 to `https://filestore.fortinet.com/fortiguard/rss/ir.xml` → 200, `content-type: text/xml`, `content-length: 38414`, `etag: "6a9c3904-960e"`, `last-modified: Sat, 05 Sep 2026 15:45:08 GMT` |
| Original shape | RSS 2.0 (`<rss version="2.0">`), generator `python-feedgen`, 50 `<item>` elements, each with exactly `title`, `link`, `description` (CDATA), `guid` and `pubDate` — no `category`, `author` or `enclosure`. Every `pubDate` is RFC 1123 with a real day-month-year and a time component of exactly `00:00:00`. Items are **not** in `pubDate` order (item 1 is `2022-04-01`; items 2 and 3 are `2026-08-12`). |
| This file | 3 of the 50 items, in the vendor's own document order |
| Expected output | `psirt-advisories.expected.json` |

**This file is hand-written to the measured shape. It is not a stored copy of
the feed.** Fortinet's terms review is `pending` (ADR-0018 — a pending source is
not collected, and re-fetching the feed in order to record a fixture would
itself be an act of collection). ADR-0009's excerpt discipline says FirmScout
keeps only what verification needs, not a reproduction of the source. So this
file was built by hand from the recon measurement above, not saved from a live
response.

What was kept, and why:

- **`title`, `link`, `guid` and `pubDate` for the three items are the vendor's
  real, measured values** (`FG-IR-22-059`, `FG-IR-26-163`, `FG-IR-26-158`), because
  those are exactly the fields the collector config extracts and the fields a
  reader would use to verify this fixture against the real feed later.
- **Every `<description>` is a placeholder**, clearly marked as such
  (`Placeholder — the vendor's advisory summary is not reproduced in this
  fixture.`), because the collector config never reads `description`
  (`evidence_excerpt: title`) and the vendor's advisory prose is not needed to
  exercise extraction.
- **The channel-level `<description>`, `<docs>` and `<generator>` are likewise
  placeholders or structural boilerplate**, kept only so the document is a
  well-formed RSS 2.0 feed, not a reproduction of the channel's real metadata.
- **Document order is preserved as measured** — item 1 is the oldest
  (`2022-04-01`) and items 2–3 are both `2026-08-12` — because
  `TestFortinetPSIRTFixtures` (A2) asserts the engine never reorders entries.

## Why this feed can never publish a release from this fixture

The PSIRT feed states that an advisory exists, its identifier, and when it was
published. It does **not** state which Fortinet products the advisory affects
— that fact lives on the advisory's own page, not in the feed. So a candidate
extracted from this source can honestly assert "Fortinet published advisory
FG-IR-26-163 on 2026-08-12" and nothing more.

The collector config's `confidence.base` (0.55) is set below the 0.85
automatic-publication threshold specifically so that validation gate 8 routes
every candidate from this source to human review, and
`domain.Release.EligibleForLatest()` refuses `advisory`-type releases the
latest-observed flag regardless. Phase 2 exercises the `advisory` release type
end to end through extraction, validation and review routing. It does **not**
publish a Fortinet advisory as a release, because the feed does not contain the
fact (which product is affected) that would make that publication true.
Advisory-to-product correlation is Phase 4 (`security_advisories`) and is out
of scope here.

## How the harness pairs files

For every `<name>.fixture.<ext>` there is a sibling `<name>.expected.json`. The
expected file's `collectorId` (`fortinet.psirt-advisories`) is what routes this
fixture to the shipped collector config rather than any other engine's test
corpus. `internal/adapters/collectors/rss_atom_test.go`'s
`TestFortinetPSIRTFixtures` runs the real, shipped
`collectors/config/fortinet/psirt-advisories.yaml` against this directory.

Re-record after a deliberate, reviewed change to extraction behaviour with:

```
UPDATE_FIXTURES=1 go test ./internal/adapters/collectors/... -run Fortinet -count=1
```

That rewrites the `candidates` array and leaves `note`, `collectorId` and
`warningsContain` alone. The resulting diff is something a human reads in the
pull request before it is committed — a candidate list that changed for a
reason nobody can explain is a bug that has just been committed, not fixed.
