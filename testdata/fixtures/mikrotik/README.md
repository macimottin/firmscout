# MikroTik collector fixtures

Every file in this directory is **test data**. Nothing here is seeded into a
database, published through the API, or presented anywhere as a statement about
what MikroTik has released. The recorded files are trimmed excerpts kept so that
extraction can be verified offline; the synthetic files are constructed by hand
to exercise a failure mode.

No test in this repository fetches a vendor site. A collector test suite that
could fail because a vendor redesigned their page teaches contributors to ignore
CI, which is worse than a fixture going briefly stale.

## Recorded fixtures

### `changelogs.fixture.html` — recorded, trimmed

| | |
| --- | --- |
| Source URL | `https://mikrotik.com/download/changelogs` |
| Retrieved | 2026-09-03 (UTC) |
| Original response | server-rendered HTML, roughly 409 KB, `cache-control: private`, **no `ETag`**, 46 `div.changelog-header` entries |
| This file | a trimmed excerpt: 3 of the 46 entries, in the markup shape the page actually uses |
| Expected output | `changelogs.expected.json` |

**This is an excerpt retained for verification, not a reproduction of the page.**
The original is the vendor's copyrighted content; what extraction needs is
structure, not content completeness, so everything not load-bearing for the test
was deleted. What was kept beyond the three entries — a navigation bar, a
`<style>`, a `<script>`, a collapsible entry body per release, and a footer
carrying a version-shaped string outside `div.changelog-header` — is kept
deliberately: it is what makes `normalize.strip`, the section selector, and the
container selector genuinely exercised rather than trivially satisfied. The
`<script>` contains the string `9.99.9`, so a collector that started reading
script contents would fail this fixture rather than pass it quietly.

The three entries carry the versions, channels and dates the page listed on the
retrieval date. Where the entry bodies (the changelog text itself) were kept at
all, they are reduced to a single representative line.

### `newest-stable.fixture.txt` — recorded, complete

| | |
| --- | --- |
| Source URL | `https://upgrade.mikrotik.com/routeros/NEWESTa7.stable` |
| Retrieved | 2026-09-03 (UTC) |
| Original response | `Content-Type: text/plain`, body exactly `7.24.2 1788429434`, with both `ETag` and `Last-Modified` present |
| This file | the complete body, 17 bytes, no trailing newline |
| Expected output | `newest-stable.expected.json` |

This one is not trimmed, because the entire response is a version, a space and a
Unix epoch. `1788429434` is `2026-09-03T09:57:14Z`.

## Synthetic fixtures

These are **not recordings**. They were written by hand to pin down a behaviour
that a recorded page cannot demonstrate, and each says so in a comment at the
top of the file.

| File | What it pins down |
| --- | --- |
| `layout-changed.fixture.html` | The vendor restructures the page and `div.changelog-header` stops existing. Extraction must produce zero candidates and **no error**, with a warning naming the selector that missed. A layout change is a repair signal, not a crash. |
| `empty-version.fixture.html` | A container whose version span is blank. That candidate is skipped with a warning; the well-formed entry beside it still extracts. A candidate with no version cannot be deduplicated and must never be emitted. |
| `unmapped-channel.fixture.html` | A channel badge nobody has interpreted (`Nightly`). Per collector-config-spec.md §3.6 this is a hard error for that candidate, never a pass-through: an unmapped label is not evidence for any channel. |
| `month-only-date.fixture.html` | A release dated `2026-02` and nothing more. The candidate must carry month precision and no day, and `ExactDay()` must report false. A second entry with no parsable date must land on the unknown date rather than an approximation. Read by the synthetic `synthetic.month-only` config defined in `internal/adapters/collectors/html_selectors_test.go`, because the real `mikrotik.changelogs` config declares `exact_day` — the real page really does publish full dates. |

## How the harness pairs files

For every `<name>.fixture.<ext>` there is a sibling `<name>.expected.json`. The
expected file's `collectorId` says which collector the fixture belongs to, so
one directory can hold the fixtures of several collectors for one vendor without
any of them being fed to the wrong engine.

Re-record after a deliberate change to extraction behaviour with:

```
UPDATE_FIXTURES=1 go test ./internal/adapters/collectors/... -count=1
```

That rewrites the `candidates` array and leaves the `note`, `collectorId` and
`warningsContain` fields alone. The resulting diff is something a human reads in
the pull request — a candidate list that changed for a reason nobody can explain
is a bug that has just been committed, not fixed.
