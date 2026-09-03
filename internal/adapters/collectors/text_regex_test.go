package collectors_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/collectors/sdk/collectortest"
	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/domain"
)

func newShippedTextCollector(t *testing.T, id string) *collectors.TextRegex {
	t.Helper()
	c, err := collectors.NewTextRegex(loadShippedConfig(t, id), nil)
	if err != nil {
		t.Fatalf("build collector %s: %v", id, err)
	}
	return c
}

// TestTextRegexFixtures runs the shipped MikroTik pointer-file collector against
// every fixture that declares it.
func TestTextRegexFixtures(t *testing.T) {
	collectortest.RunFixtures(t, newShippedTextCollector(t, "mikrotik.newest-stable"), fixturesDir)
}

// TestEpochSecondsConvertsToTheExactUTCDay exercises the engine's epoch_seconds
// layout, using a config written for this test rather than the shipped MikroTik one.
//
// The distinction matters. The engine is perfectly capable of turning an epoch into an
// exact day, and that capability needs a test. The shipped MikroTik config deliberately
// does NOT use it, because MikroTik never documents what the epoch in its pointer file
// means, and publishing a derived timestamp as a vendor's stated release date is
// exactly the invented precision this project forbids. Testing the capability against
// the shipped config would quietly couple a policy decision to an engine test, so that
// changing the policy looks like breaking the engine.
func TestEpochSecondsConvertsToTheExactUTCDay(t *testing.T) {
	cfg, err := collectors.LoadConfig([]byte(`
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: test.epoch
  vendor: test-vendor
  version: 1
spec:
  engine: text_regex
  product_match:
    product: test-vendor-thing
  extract:
    release_container: "(?P<version>\\S+)[ \\t]+(?P<epoch>\\d+)"
    fields:
      version:
        selector: version
      release_date:
        selector: epoch
        date_format: epoch_seconds
        precision: exact_day
    release_type: firmware
    evidence_excerpt: ":scope"
`))
	if err != nil {
		t.Fatalf("load test config: %v", err)
	}
	c, err := collectors.NewTextRegex(cfg, nil)
	if err != nil {
		t.Fatalf("build collector: %v", err)
	}

	candidates, err := c.Extract(context.Background(), fixtureSource(c), sdk.Artifact{
		ContentType: "text/plain",
		Body:        []byte("7.24.2 1788429434"),
		RetrievedAt: collectortest.FixtureRetrievedAt,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(candidates))
	}

	day, ok := candidates[0].ReleaseDate.ExactDay()
	if !ok {
		t.Fatalf("release date is %s precision, want exact_day", candidates[0].ReleaseDate.Precision())
	}
	// The day must be the UTC day, not the day in whatever zone the test machine sits in.
	want := time.Unix(1788429434, 0).UTC()
	if day.Year() != want.Year() || day.Month() != want.Month() || day.Day() != want.Day() {
		t.Errorf("release date: want %s, got %s", want.Format("2006-01-02"), day.Format("2006-01-02"))
	}
}

// TestShippedPointerCollectorPublishesNoDate pins the policy the config comment
// explains: the MikroTik pointer file yields a version and nothing more, because the
// epoch beside it is undocumented. A change that starts publishing a date from it
// should fail here and be argued for, not slip through.
func TestShippedPointerCollectorPublishesNoDate(t *testing.T) {
	c := newShippedTextCollector(t, "mikrotik.newest-stable")
	candidates, err := c.Extract(context.Background(), fixtureSource(c), sdk.Artifact{
		ContentType: "text/plain",
		Body:        []byte("7.24.2 1788429434"),
		RetrievedAt: collectortest.FixtureRetrievedAt,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(candidates))
	}
	got := candidates[0]
	if got.Version.Raw() != "7.24.2" {
		t.Errorf("version: want 7.24.2, got %q", got.Version.Raw())
	}
	if got.Applicability.Channel != "stable" {
		t.Errorf("channel: want stable (fixed by the config), got %q", got.Applicability.Channel)
	}
	if got.ReleaseDate.Known() {
		t.Errorf("the pointer collector published a release date (%s at %s precision); "+
			"MikroTik does not document what the epoch means, so no date may be asserted from it",
			got.ReleaseDate.String(), got.ReleaseDate.Precision())
	}
	if _, ok := got.ReleaseDate.ExactDay(); ok {
		t.Error("an exact day was derived from an undocumented epoch")
	}
}

// TestTextRegexNoMatchIsNotAnError: an endpoint that returns something unexpected
// produces zero candidates and a warning, not an error. The endpoint returning a
// maintenance page is a repair signal, not an exception.
func TestTextRegexNoMatchIsNotAnError(t *testing.T) {
	c := newShippedTextCollector(t, "mikrotik.newest-stable")
	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), sdk.Artifact{
		ContentType: "text/plain",
		Body:        []byte("service temporarily unavailable"),
		RetrievedAt: collectortest.FixtureRetrievedAt,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("want 0 candidates, got %d", len(candidates))
	}
	if !containsSubstr(warnings, "matched nothing") {
		t.Errorf("expected a warning saying the pattern matched nothing; got %v", warnings)
	}
}

// TestTextRegexBoundsItsInput proves the engine's own ceiling applies even when the
// config asks for more, and that the truncation is reported rather than silent.
func TestTextRegexBoundsItsInput(t *testing.T) {
	cfg, err := collectors.LoadConfig([]byte(`
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata: {id: synthetic.bounded, vendor: mikrotik, version: 1}
spec:
  engine: text_regex
  product_match: {product: mikrotik-routeros}
  fetch: {max_bytes: 64}
  extract:
    release_container: "(?P<version>[0-9]+\\.[0-9]+\\.[0-9]+)"
    fields:
      version: {selector: version}
    release_type: embedded_os
    channel: stable
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	c, err := collectors.NewTextRegex(cfg, nil)
	if err != nil {
		t.Fatalf("build collector: %v", err)
	}

	// 64 bytes of padding, then a version that only exists past the limit.
	body := strings.Repeat("x", 64) + " 9.9.9"
	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), sdk.Artifact{
		ContentType: "text/plain",
		Body:        []byte(body),
		RetrievedAt: collectortest.FixtureRetrievedAt,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("want 0 candidates from beyond the byte limit, got %d", len(candidates))
	}
	if !containsSubstr(warnings, "truncated") {
		t.Errorf("expected a warning reporting the truncation; got %v", warnings)
	}
}

// TestTextRegexCandidateIsDomainValid checks the emitted candidate against the
// domain's own invariants once the application layer's fields are stamped.
func TestTextRegexCandidateIsDomainValid(t *testing.T) {
	c := newShippedTextCollector(t, "mikrotik.newest-stable")
	candidates, err := c.Extract(context.Background(), fixtureSource(c), sdk.Artifact{
		ContentType: "text/plain",
		Body:        []byte("7.24.2 1788429434"),
		RetrievedAt: collectortest.FixtureRetrievedAt,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, cand := range candidates {
		cand.SourceID = "src_test"
		cand.State = domain.CandidateExtracted
		if err := cand.Validate(); err != nil {
			t.Errorf("candidate %s fails domain validation: %v", cand.Version.Raw(), err)
		}
	}
}
