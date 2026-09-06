# `rss_atom` engine fixtures

Every file in this directory is **synthetic**: hand-written to exercise one
behaviour of the `rss_atom` engine (collector-config-spec.md §4.4), not recorded
from any real vendor feed. `test-vendor` and `example.test` name nothing that
exists. The engine's *shipped* worked example — the real Fortinet PSIRT feed,
recorded to the measured shape and reviewed by B2 — lives at
`testdata/fixtures/fortinet/`, not here.

No test in this repository fetches a live feed. A collector test suite that could
fail because a vendor changed their feed teaches contributors to ignore CI, which
is worse than a fixture going briefly stale.

## What each fixture proves

| Fixture | Collector (see `rss_atom_test.go`) | Proves |
| --- | --- | --- |
| `rss20.fixture.xml` | `test.rss-basic` | RSS 2.0: a version captured from the title, an exact-day `pubDate`, a mapped `category`. |
| `unmapped-channel.fixture.xml` | `test.rss-basic` | A `category` badge with no map entry (`Nightly`) hard-fails that one candidate with a warning; the other entry is unaffected. |
| `empty.fixture.xml` | `test.rss-basic` | A feed with no `<item>` elements is a valid feed: zero candidates, a warning, no error. |
| `dialect-mismatch.fixture.xml` | `test.rss-basic` | The collector declares `release_container: item`; this document is Atom. Zero candidates, a warning naming both. |
| `malformed.fixture.xml` | `test.rss-basic` | An unclosed `<item>`, document truncated mid-tag. A parse failure is an error, never a partial silent result. |
| `atom.fixture.xml` | `test.atom-basic` | Atom: `link/@href` resolution (skipping the `rel="self"` link), a month-only `<published>` date, and — the second entry, which carries only `<updated>` — that `pub_date` never falls back to it (D14). |

`rss_atom_test.go` also exercises the reverse container mismatch (an
Atom-declared collector fed `rss20.fixture.xml`) directly, without a matching
`.expected.json`, since it only needs to assert a warning and zero candidates.
