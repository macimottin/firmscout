package collectors_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/collectors/sdk/collectortest"
	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/domain"
)

const (
	repoRoot    = "../../.."
	configRoot  = "collectors/config"
	fixturesDir = "../../../testdata/fixtures/mikrotik"
)

// syntheticMonthOnlyConfig is a test-only config, not a shipped one.
//
// It exists because the real mikrotik.changelogs config declares
// precision: exact_day -- correctly, because the real page publishes full dates --
// so it cannot demonstrate the month-precision path. The markup it reads is the
// synthetic month-only-date fixture.
const syntheticMonthOnlyConfig = `
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: synthetic.month-only
  vendor: mikrotik
  version: 1
spec:
  engine: html_selectors
  product_match:
    product: mikrotik-routeros
  normalize:
    section_selector: "div.changelog-header"
    strip: [script, style]
  extract:
    release_container: "div.changelog-header"
    fields:
      version:
        selector: "span.font-bold"
        transform: trim
      channel:
        selector: "div.uppercase > div"
        transform: [trim, lowercase]
        map:
          long-term: long_term
          stable: stable
      release_date:
        selector: ":scope"
        regex: "(\\d{4}-\\d{2})"
        date_format: "2006-01"
        precision: month_only
    release_type: embedded_os
    evidence_excerpt: ":scope"
`

// loadShippedConfig returns the checked-in config with the given id.
func loadShippedConfig(t *testing.T, id string) collectors.Config {
	t.Helper()
	configs, err := collectors.LoadDir(os.DirFS(repoRoot), configRoot)
	if err != nil {
		t.Fatalf("load %s: %v", configRoot, err)
	}
	for _, cfg := range configs {
		if cfg.Metadata.ID == id {
			return cfg
		}
	}
	t.Fatalf("no config with id %q under %s", id, configRoot)
	return collectors.Config{}
}

func newShippedHTMLCollector(t *testing.T, id string) *collectors.HTMLSelectors {
	t.Helper()
	c, err := collectors.NewHTMLSelectors(loadShippedConfig(t, id), nil)
	if err != nil {
		t.Fatalf("build collector %s: %v", id, err)
	}
	return c
}

func newSyntheticMonthOnlyCollector(t *testing.T) *collectors.HTMLSelectors {
	t.Helper()
	cfg, err := collectors.LoadConfig([]byte(syntheticMonthOnlyConfig))
	if err != nil {
		t.Fatalf("load synthetic month-only config: %v", err)
	}
	c, err := collectors.NewHTMLSelectors(cfg, nil)
	if err != nil {
		t.Fatalf("build synthetic month-only collector: %v", err)
	}
	return c
}

// TestHTMLSelectorsFixtures runs the shipped MikroTik changelog collector against
// every fixture that declares it.
func TestHTMLSelectorsFixtures(t *testing.T) {
	collectortest.RunFixtures(t, newShippedHTMLCollector(t, "mikrotik.changelogs"), fixturesDir)
}

// TestHTMLSelectorsMonthOnlyFixture runs the synthetic month-precision collector.
func TestHTMLSelectorsMonthOnlyFixture(t *testing.T) {
	collectortest.RunFixtures(t, newSyntheticMonthOnlyCollector(t), fixturesDir)
}

// TestMonthOnlyDateInventsNoDay is the explicit statement of the rule the fixture
// only implies: a config that declares month_only produces a month-precision date,
// and there is no way to read a day out of it.
//
// This is worth its own test rather than being left to the fixture comparison
// because the fixture compares PartialDate.String(), and a bug that produced an
// exact-day date anchored on the first of the month would still render "2026-02" if
// String() were ever changed to be precision-blind. Asserting on Precision() and
// ExactDay() tests the value, not its rendering.
func TestMonthOnlyDateInventsNoDay(t *testing.T) {
	candidates := extractFixture(t, newSyntheticMonthOnlyCollector(t), "month-only-date.fixture.html")
	if len(candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(candidates))
	}

	dated := candidates[0]
	if got := dated.ReleaseDate.Precision(); got != domain.PrecisionMonthOnly {
		t.Errorf("release date precision: want %q, got %q", domain.PrecisionMonthOnly, got)
	}
	if day, ok := dated.ReleaseDate.ExactDay(); ok {
		t.Errorf("ExactDay() returned %s: a day was invented from a month-only source", day.Format("2006-01-02"))
	}
	if year, ok := dated.ReleaseDate.Year(); !ok || year != 2026 {
		t.Errorf("year: want 2026 known, got %d known=%v", year, ok)
	}
	if month, ok := dated.ReleaseDate.Month(); !ok || month.String() != "February" {
		t.Errorf("month: want February known, got %v known=%v", month, ok)
	}
	if got := dated.ReleaseDate.String(); got != "2026-02" {
		t.Errorf("rendered date: want %q, got %q", "2026-02", got)
	}

	// The second entry's date text cannot be parsed at all. It must be unknown, not
	// approximated to the first of some month.
	unparsable := candidates[1]
	if got := unparsable.ReleaseDate.Precision(); got != domain.PrecisionUnknown {
		t.Errorf("unparsable date precision: want %q, got %q", domain.PrecisionUnknown, got)
	}
	if unparsable.ReleaseDate.Known() {
		t.Errorf("unparsable date reports itself known as %q", unparsable.ReleaseDate.String())
	}
	if got := unparsable.ReleaseDate.String(); got != "" {
		t.Errorf("unparsable date renders as %q; it must render as nothing at all", got)
	}
	// A missing optional signal lowers confidence rather than discarding the
	// candidate: a release with an unknown date is still a release.
	if unparsable.Confidence >= dated.Confidence {
		t.Errorf("confidence: an unknown date should lower it (dated %.2f, unparsable %.2f)",
			dated.Confidence, unparsable.Confidence)
	}
}

// TestLayoutChangedYieldsNoCandidatesAndNoError pins down the behaviour a vendor
// redesign must produce. Returning an error here would turn every layout change into
// a failed job with retries and an alert; returning candidates would be worse.
func TestLayoutChangedYieldsNoCandidatesAndNoError(t *testing.T) {
	c := newShippedHTMLCollector(t, "mikrotik.changelogs")
	body, err := os.ReadFile(fixturesDir + "/layout-changed.fixture.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), fixtureArtifact(body))
	if err != nil {
		t.Fatalf("Extract returned an error for a page whose layout changed: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("want 0 candidates, got %d", len(candidates))
	}
	if !containsSubstr(warnings, "matched no elements") {
		t.Errorf("expected a warning naming the selector that missed; got %v", warnings)
	}
}

// TestEmptyVersionIsSkippedWithAReason proves the skip is loud and scoped: the
// malformed entry is dropped with an explanation, and the entry beside it survives.
func TestEmptyVersionIsSkippedWithAReason(t *testing.T) {
	c := newShippedHTMLCollector(t, "mikrotik.changelogs")
	body, err := os.ReadFile(fixturesDir + "/empty-version.fixture.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), fixtureArtifact(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 surviving candidate, got %d", len(candidates))
	}
	if got := candidates[0].Version.Raw(); got != "7.21.5" {
		t.Errorf("surviving candidate version: want 7.21.5, got %q", got)
	}
	if !containsSubstr(warnings, `field "version" produced no value`) {
		t.Errorf("expected a warning explaining the skip; got %v", warnings)
	}
	for _, c := range candidates {
		if c.Version.IsZero() {
			t.Error("a candidate with an empty version was emitted")
		}
	}
}

// TestUnmappedChannelIsAHardErrorForThatCandidate: an unrecognised badge fails
// loudly for the candidate that carried it, and only for that candidate.
func TestUnmappedChannelIsAHardErrorForThatCandidate(t *testing.T) {
	c := newShippedHTMLCollector(t, "mikrotik.changelogs")
	body, err := os.ReadFile(fixturesDir + "/unmapped-channel.fixture.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), fixtureArtifact(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(candidates))
	}
	if got := candidates[0].Applicability.Channel; got != "stable" {
		t.Errorf("surviving candidate channel: want stable, got %q", got)
	}
	if !containsSubstr(warnings, "has no entry in the field's map") {
		t.Errorf("expected a warning naming the unmapped label; got %v", warnings)
	}
}

// TestExtractIsPure runs the same artifact through the same collector twice and
// requires byte-identical output. It is the executable form of the SDK's central
// claim: no clock, no repository, no network, therefore no drift.
func TestExtractIsPure(t *testing.T) {
	c := newShippedHTMLCollector(t, "mikrotik.changelogs")
	first := extractFixture(t, c, "changelogs.fixture.html")
	second := extractFixture(t, c, "changelogs.fixture.html")

	if len(first) != len(second) {
		t.Fatalf("two runs produced different candidate counts: %d and %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("candidate %d differs between two runs of the same artifact:\n  %+v\n  %+v", i, first[i], second[i])
		}
	}
}

// TestDedupeKeysAreStableAndDistinct: a key that changed between runs would create a
// duplicate candidate on every check; a key shared by two different releases would
// silently swallow one of them.
func TestDedupeKeysAreStableAndDistinct(t *testing.T) {
	c := newShippedHTMLCollector(t, "mikrotik.changelogs")
	candidates := extractFixture(t, c, "changelogs.fixture.html")

	seen := make(map[string]string, len(candidates))
	for _, cand := range candidates {
		if cand.DedupeKey == "" {
			t.Fatalf("candidate %s has no dedupe key", cand.Version.Raw())
		}
		if prev, dup := seen[cand.DedupeKey]; dup {
			t.Errorf("versions %s and %s share dedupe key %s", prev, cand.Version.Raw(), cand.DedupeKey)
		}
		seen[cand.DedupeKey] = cand.Version.Raw()

		want := domain.ComputeDedupeKey(cand.ProductMatchHint, cand.Version, cand.Applicability)
		if cand.DedupeKey != want {
			t.Errorf("dedupe key for %s is not domain.ComputeDedupeKey's: %s vs %s",
				cand.Version.Raw(), cand.DedupeKey, want)
		}
	}
}

// TestCandidatesSatisfyDomainValidation checks that what a collector emits is
// acceptable to the domain once the application layer stamps the fields extraction
// deliberately does not own (id, source, state).
func TestCandidatesSatisfyDomainValidation(t *testing.T) {
	c := newShippedHTMLCollector(t, "mikrotik.changelogs")
	for _, cand := range extractFixture(t, c, "changelogs.fixture.html") {
		cand.SourceID = "src_test"
		cand.State = domain.CandidateExtracted
		if err := cand.Validate(); err != nil {
			t.Errorf("candidate %s fails domain validation: %v", cand.Version.Raw(), err)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractFixture(t *testing.T, c sdk.Collector, name string) []domain.CandidateRelease {
	t.Helper()
	body, err := os.ReadFile(fixturesDir + "/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	candidates, err := c.Extract(context.Background(), fixtureSource(c), fixtureArtifact(body))
	if err != nil {
		t.Fatalf("extract %s: %v", name, err)
	}
	return candidates
}

func fixtureSource(c sdk.Collector) domain.Source {
	return domain.Source{
		ID:          "src_test",
		Slug:        "fixture",
		CollectorID: c.ID(),
		SourceType:  domain.SourceTypeHTMLPage,
		URL:         "fixture://local",
	}
}

func fixtureArtifact(body []byte) sdk.Artifact {
	return sdk.Artifact{
		ID:          "art_test",
		SourceID:    "src_test",
		ContentType: "text/html; charset=utf-8",
		Body:        body,
		RetrievedAt: collectortest.FixtureRetrievedAt,
		URL:         "fixture://local",
	}
}

func containsSubstr(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
