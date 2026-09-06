package collectors_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/collectors/sdk/collectortest"
	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// rssAtomFixturesDir holds this engine's own fixture corpus (collector-config-spec.md
// §4.4), distinct from testdata/fixtures/<vendor> which is B2's real-vendor corpus. It
// is a directory of this package, so the path is relative to it rather than to the repo
// root the way fixturesDir (html_selectors_test.go) is.
const rssAtomFixturesDir = "testdata/rss_atom"

// testRSSConfig is a synthetic RSS 2.0 config, not a shipped vendor one. It declares
// release_container: item explicitly so the dialect-mismatch fixture (an Atom document)
// has something to disagree with.
const testRSSConfig = `
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: test.rss-basic
  vendor: test-vendor
  version: 1
spec:
  engine: rss_atom
  product_match:
    product: test-vendor-thing
  extract:
    release_container: item
    fields:
      version:
        selector: title
        regex: "([0-9]+\\.[0-9]+\\.[0-9]+)"
        transform: trim
      channel:
        selector: category
        transform: [trim, lowercase]
        map:
          stable: stable
          "long-term": long_term
      release_date:
        selector: pub_date
        date_format: "Mon, 02 Jan 2006 15:04:05 -0700"
        precision: exact_day
      release_notes_url:
        selector: link
        transform: trim
    release_type: embedded_os
    evidence_excerpt: title
`

// testAtomConfig is a synthetic Atom config exercising link/@href resolution and a
// month-only <published> date, plus the D14 no-fallback-to-<updated> rule.
const testAtomConfig = `
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: test.atom-basic
  vendor: test-vendor
  version: 1
spec:
  engine: rss_atom
  product_match:
    product: test-vendor-thing
  extract:
    release_container: entry
    fields:
      version:
        selector: title
        regex: "([0-9]+\\.[0-9]+)"
        transform: trim
      release_notes_url:
        selector: link
        transform: trim
      release_date:
        selector: pub_date
        date_format: "January 2006"
        precision: month_only
    release_type: embedded_os
    evidence_excerpt: title
`

func newSyntheticRSSAtomCollector(t *testing.T, yamlDoc string) *collectors.RSSAtom {
	t.Helper()
	cfg, err := collectors.LoadConfig([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("load synthetic rss_atom config: %v", err)
	}
	c, err := collectors.NewRSSAtom(cfg, nil)
	if err != nil {
		t.Fatalf("build synthetic rss_atom collector: %v", err)
	}
	return c
}

func newTestRSSCollector(t *testing.T) *collectors.RSSAtom {
	t.Helper()
	return newSyntheticRSSAtomCollector(t, testRSSConfig)
}

func newTestAtomCollector(t *testing.T) *collectors.RSSAtom {
	t.Helper()
	return newSyntheticRSSAtomCollector(t, testAtomConfig)
}

// TestRSSAtomExtractsRSS20 and TestRSSAtomExtractsAtom, together with the rest of the
// fixtures under rssAtomFixturesDir, are the engine's fixture corpus required by
// collector-config-spec.md §4.4 and §8: a well-formed RSS 2.0 feed, a well-formed Atom
// feed, an unmapped channel label, a zero-entry feed, a container/dialect mismatch and a
// malformed document. RunFixtures runs every *.fixture.* file in the directory and
// filters by collectorId, so both collectors below share one directory exactly the way
// html_selectors_test.go's synthetic month-only collector shares the MikroTik fixtures
// directory.
func TestRSSAtomExtractsRSS20(t *testing.T) {
	collectortest.RunFixtures(t, newTestRSSCollector(t), rssAtomFixturesDir)
}

func TestRSSAtomExtractsAtom(t *testing.T) {
	collectortest.RunFixtures(t, newTestAtomCollector(t), rssAtomFixturesDir)
}

// TestRSSAtomPubDateDoesNotFallBackToUpdated is D14's revert detector, stated as its
// own assertion rather than left to the fixture comparison alone: an Atom entry with
// only <updated> and a config selecting pub_date must yield an unknown release date,
// never the updated value reinterpreted as a publication date.
func TestRSSAtomPubDateDoesNotFallBackToUpdated(t *testing.T) {
	c := newTestAtomCollector(t)
	body, err := os.ReadFile(rssAtomFixturesDir + "/atom.fixture.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), xmlArtifact(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(candidates))
	}

	dated := candidates[0]
	if got := dated.ReleaseDate.Precision(); got != domain.PrecisionMonthOnly {
		t.Errorf("first entry's release date precision: want %q, got %q", domain.PrecisionMonthOnly, got)
	}

	updatedOnly := candidates[1]
	if got := updatedOnly.ReleaseDate.Precision(); got != domain.PrecisionUnknown {
		t.Errorf("second entry's release date precision: want %q, got %q", domain.PrecisionUnknown, got)
	}
	if updatedOnly.ReleaseDate.Known() {
		t.Errorf("an entry with only <updated> produced a known release date: %s", updatedOnly.ReleaseDate.String())
	}
	if !containsSubstr(warnings, `field "release_date" produced no value`) {
		t.Errorf("expected a warning explaining the missing release date; got %v", warnings)
	}
}

// TestRSSAtomRejectsUnknownSelector is the runtime half of D14's closed vocabulary
// (the config_test.go table covers the rest): a selector outside the ten names fails
// to load with an error naming the vocabulary, not a collector that silently never
// locates anything.
func TestRSSAtomRejectsUnknownSelector(t *testing.T) {
	bad := strings.Replace(testRSSConfig, "selector: title\n        regex:", "selector: summary\n        regex:", 1)
	if bad == testRSSConfig {
		t.Fatal("test mutation did not apply")
	}
	_, err := collectors.LoadConfig([]byte(bad))
	if err == nil {
		t.Fatal("a selector outside the rss_atom vocabulary was accepted")
	}
	if !strings.Contains(err.Error(), "not one of the rss_atom feed fields") {
		t.Errorf("error does not name the vocabulary: %v", err)
	}
}

// TestRSSAtomRejectsHTMLOnlyNormalizeKeys: section_selector, strip and replace are all
// meaningless against a parsed feed, and each fails to load rather than being ignored.
func TestRSSAtomRejectsHTMLOnlyNormalizeKeys(t *testing.T) {
	cases := []struct {
		name   string
		inject string
		want   string
	}{
		{"section_selector", "  normalize:\n    section_selector: \"div.entry\"\n", "is only meaningful for the html_selectors engine"},
		{"strip", "  normalize:\n    strip: [script]\n", "is only meaningful for the html_selectors engine"},
		{"replace", "  normalize:\n    replace: [{pattern: \"a\", with: \"b\"}]\n", "is not supported for the rss_atom engine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(testRSSConfig, "  extract:", tc.inject+"  extract:", 1)
			if doc == testRSSConfig {
				t.Fatal("test mutation did not apply")
			}
			_, err := collectors.LoadConfig([]byte(doc))
			if err == nil {
				t.Fatalf("%s was accepted under rss_atom", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestRSSAtomContainerDialectMismatch exercises the reverse direction of the
// dialect-mismatch fixture: an Atom-declared collector fed an RSS document.
func TestRSSAtomContainerDialectMismatch(t *testing.T) {
	c := newTestAtomCollector(t)
	body, err := os.ReadFile(rssAtomFixturesDir + "/rss20.fixture.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), xmlArtifact(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("want 0 candidates, got %d", len(candidates))
	}
	if !containsSubstr(warnings, `release_container "entry" was declared but the document is RSS 2.0`) {
		t.Errorf("expected a warning naming both what was asked for and what was found; got %v", warnings)
	}
}

// TestRSSAtomIsNoLongerARoadmapEngine is W3's revert detector: a minimal rss_atom
// document loads, and the registry builds a real collector for it rather than refusing
// it as a reserved-but-unimplemented engine name.
func TestRSSAtomIsNoLongerARoadmapEngine(t *testing.T) {
	cfg, err := collectors.LoadConfig([]byte(testRSSConfig))
	if err != nil {
		t.Fatalf("minimal rss_atom config was rejected: %v", err)
	}
	if cfg.Spec.Engine != collectors.EngineRSSAtom {
		t.Fatalf("engine: want %q, got %q", collectors.EngineRSSAtom, cfg.Spec.Engine)
	}

	r, err := collectors.NewRegistry([]collectors.Config{cfg})
	if err != nil {
		t.Fatalf("registry refused to build an rss_atom collector: %v", err)
	}
	src := domain.Source{ID: "src_1", Slug: "test", CollectorID: "test.rss-basic"}
	got, err := r.For(src)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ID() != "test.rss-basic" {
		t.Errorf("resolved collector id: want test.rss-basic, got %q", got.ID())
	}
	var _ = application.Collector(got)
}

// TestRSSAtomPrecisionStillRejectsAMonthLayoutWithExactDay proves the shared §6
// date-precision consistency check (config.go's validateDateField) is reachable
// through this engine's own field-loading path, not just through html_selectors and
// text_regex.
func TestRSSAtomPrecisionStillRejectsAMonthLayoutWithExactDay(t *testing.T) {
	bad := strings.Replace(testRSSConfig,
		`date_format: "Mon, 02 Jan 2006 15:04:05 -0700"
        precision: exact_day`,
		`date_format: "January 2006"
        precision: exact_day`, 1)
	if bad == testRSSConfig {
		t.Fatal("test mutation did not apply")
	}
	_, err := collectors.LoadConfig([]byte(bad))
	if err == nil {
		t.Fatal("exact_day paired with a month-only layout was accepted")
	}
	if !strings.Contains(err.Error(), "carries no day-of-month component") {
		t.Errorf("error does not explain the mismatch: %v", err)
	}
}

// TestRSSAtomCandidatesSatisfyDomainValidation checks emitted candidates against the
// domain's own invariants once the application layer's fields are stamped, mirroring
// the equivalent test for the other two engines.
func TestRSSAtomCandidatesSatisfyDomainValidation(t *testing.T) {
	c := newTestRSSCollector(t)
	body, err := os.ReadFile(rssAtomFixturesDir + "/rss20.fixture.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	candidates, err := c.Extract(context.Background(), fixtureSource(c), xmlArtifact(body))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("want at least one candidate")
	}
	for _, cand := range candidates {
		cand.SourceID = "src_test"
		cand.State = domain.CandidateExtracted
		if err := cand.Validate(); err != nil {
			t.Errorf("candidate %s fails domain validation: %v", cand.Version.Raw(), err)
		}
	}
}

// TestFortinetPSIRTFixtures runs the shipped fortinet.psirt-advisories config
// (collectors/config/fortinet, B2) against its fixture corpus (testdata/fixtures/fortinet,
// B2). Per the integration order (spec §9.2), B2 lands after this engine, so both are
// allowed to not exist yet -- this test skips visibly rather than failing the whole
// suite on a file this package does not own, exactly the way the PostgreSQL adapter
// tests skip visibly when their database is not configured.
func TestFortinetPSIRTFixtures(t *testing.T) {
	const (
		fortinetConfigPath  = "../../../collectors/config/fortinet/psirt-advisories.yaml"
		fortinetFixturesDir = "../../../testdata/fixtures/fortinet"
	)
	if _, err := os.Stat(fortinetConfigPath); os.IsNotExist(err) {
		t.Skipf("SKIP (visibly): %s does not exist yet -- this is B2's file, landing after the rss_atom engine per the integration order", fortinetConfigPath)
	}
	if _, err := os.Stat(fortinetFixturesDir); os.IsNotExist(err) {
		t.Skipf("SKIP (visibly): %s does not exist yet -- this is B2's fixture corpus, landing after the rss_atom engine per the integration order", fortinetFixturesDir)
	}

	data, err := os.ReadFile(fortinetConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", fortinetConfigPath, err)
	}
	cfg, err := collectors.LoadConfig(data)
	if err != nil {
		t.Fatalf("load %s: %v", fortinetConfigPath, err)
	}
	c, err := collectors.NewRSSAtom(cfg, nil)
	if err != nil {
		t.Fatalf("build fortinet.psirt-advisories collector: %v", err)
	}
	collectortest.RunFixtures(t, c, fortinetFixturesDir)
}

// xmlArtifact builds an artifact the way collectortest would for an .xml fixture, for
// tests that read a fixture file directly rather than going through RunFixtures.
func xmlArtifact(body []byte) sdk.Artifact {
	return sdk.Artifact{
		ID:          "art_test",
		SourceID:    "src_test",
		ContentType: "application/xml",
		Body:        body,
		RetrievedAt: collectortest.FixtureRetrievedAt,
		URL:         "fixture://local",
	}
}
