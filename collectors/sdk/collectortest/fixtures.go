// Package collectortest is the fixture harness every FirmScout collector is tested
// with, whether it is generated from a YAML configuration or hand-written.
//
// The harness only ever calls Extract. It never fetches, never opens a socket, and
// never consults a clock: a fixture test that could fail because a vendor redesigned
// their site teaches contributors to ignore CI, which is a worse outcome than a
// briefly stale fixture. See docs/architecture/collector-sdk.md §8 and §9.
package collectortest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/internal/domain"
)

// UpdateEnvVar is the environment variable that rewrites expected files instead of
// failing on a mismatch:
//
//	UPDATE_FIXTURES=1 go test ./internal/adapters/collectors/...
//
// It is a convenience for the common case of a deliberate, reviewed change to
// extraction behaviour. It is emphatically not a way to make a failing test pass: the
// rewritten file is a diff a human reads in the pull request, and a candidate list
// that changed for a reason nobody can explain is a bug that has just been committed
// rather than fixed.
const UpdateEnvVar = "UPDATE_FIXTURES"

// FixtureRetrievedAt is the retrieval timestamp stamped on every fixture artifact.
//
// It is a constant, not time.Now(), because Extract must be a pure function of its
// inputs: a harness that stamped the current time would make the artifact different on
// every run, which is exactly the property fixtures exist to rule out.
var FixtureRetrievedAt = time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)

// Options tunes what the harness feeds a collector. The zero value is what
// RunFixtures uses and is right for a config-driven collector.
type Options struct {
	// Source overrides the synthetic source handed to Extract. When it is the zero
	// value, the harness builds one whose CollectorID is c.ID(), which is what makes
	// Supports return true for a config-driven collector.
	Source domain.Source
	// RetrievedAt overrides FixtureRetrievedAt.
	RetrievedAt time.Time
	// URL overrides the synthetic artifact URL.
	URL string
}

// RunFixtures runs c against every fixture in dir and compares the result with that
// fixture's expected file.
//
// Layout: for each `<name>.fixture.<ext>` there is a sibling `<name>.expected.json`.
// The extension selects the artifact's Content-Type (.html, .htm, .txt, .json, .xml).
// A fixture whose expected file declares a `collectorId` other than c.ID() is skipped,
// so one directory can hold the fixtures of several collectors for one vendor without
// each of them being fed to the wrong engine.
//
// The harness fails when no fixture ran at all, which is what catches the two silent
// failure modes: a wrong directory, and a `collectorId` typo that would otherwise make
// a whole suite vacuously green.
func RunFixtures(t *testing.T, c sdk.Collector, dir string) {
	t.Helper()
	RunFixturesWithOptions(t, c, dir, Options{})
}

// RunFixturesWithOptions is RunFixtures with control over the synthetic source.
func RunFixturesWithOptions(t *testing.T, c sdk.Collector, dir string, opts Options) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "*.fixture.*"))
	if err != nil {
		t.Fatalf("collectortest: glob %s: %v", dir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("collectortest: no *.fixture.* files under %s", dir)
	}
	sort.Strings(matches)

	ran := 0
	for _, fixturePath := range matches {
		name := fixtureName(fixturePath)
		expectedPath := filepath.Join(dir, name+".expected.json")

		exp, err := loadExpected(expectedPath)
		if err != nil {
			t.Errorf("collectortest: %s: %v", filepath.Base(expectedPath), err)
			continue
		}
		if exp.CollectorID != "" && exp.CollectorID != c.ID() {
			continue // belongs to a different collector in the same directory
		}
		ran++
		t.Run(name, func(t *testing.T) {
			runOne(t, c, fixturePath, expectedPath, exp, opts)
		})
	}

	if ran == 0 {
		t.Fatalf("collectortest: no fixture in %s targets collector %q "+
			"(check the collectorId field in the *.expected.json files)", dir, c.ID())
	}
}

func runOne(t *testing.T, c sdk.Collector, fixturePath, expectedPath string, exp expectedFile, opts Options) {
	t.Helper()

	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	src := opts.Source
	if src.CollectorID == "" {
		src = syntheticSource(c, fixturePath)
	}
	retrievedAt := opts.RetrievedAt
	if retrievedAt.IsZero() {
		retrievedAt = FixtureRetrievedAt
	}
	url := opts.URL
	if url == "" {
		url = "fixture://" + filepath.Base(fixturePath)
	}

	art := sdk.Artifact{
		ID:          "art_fixture",
		SourceID:    src.ID,
		ContentType: contentTypeFor(fixturePath),
		Body:        body,
		RetrievedAt: retrievedAt,
		URL:         url,
	}

	got, warnings, err := sdk.Warnings(context.Background(), c, src, art)
	if err != nil {
		if exp.Error == "" {
			t.Fatalf("Extract returned an unexpected error: %v", err)
		}
		if !strings.Contains(err.Error(), exp.Error) {
			t.Fatalf("Extract error %q does not contain the expected %q", err.Error(), exp.Error)
		}
		return
	}
	if exp.Error != "" {
		t.Fatalf("expected an error containing %q, but Extract succeeded with %d candidates", exp.Error, len(got))
	}

	if os.Getenv(UpdateEnvVar) != "" {
		if err := writeExpected(expectedPath, exp, got); err != nil {
			t.Fatalf("update %s: %v", filepath.Base(expectedPath), err)
		}
		t.Logf("%s=%s: rewrote %s with %d candidates -- review this diff, do not merge it unread",
			UpdateEnvVar, os.Getenv(UpdateEnvVar), filepath.Base(expectedPath), len(got))
		return
	}

	if diff := compare(exp.Candidates, got); diff != "" {
		t.Errorf("candidates from %s do not match %s:\n%s\n\nRe-record with: %s=1 go test ./... -run %s",
			filepath.Base(fixturePath), filepath.Base(expectedPath), diff, UpdateEnvVar, t.Name())
	}

	for _, want := range exp.WarningsContain {
		if !containsSubstring(warnings, want) {
			t.Errorf("expected a warning containing %q; got %s", want, renderWarnings(warnings))
		}
	}
}

// ---------------------------------------------------------------------------
// Expected-file format
// ---------------------------------------------------------------------------

// expectedFile is the on-disk shape of a `<name>.expected.json`, matching
// docs/architecture/collector-sdk.md §8.
//
// The split between exact and loose assertions is deliberate. Version, date, precision,
// release type, channel and product hint are compared exactly: those are the facts
// FirmScout publishes, and a change to any of them is a change a reviewer must see.
// The excerpt and the confidence score are compared loosely -- excerpt boundaries and
// a confidence float should be free to improve without rewriting every fixture in the
// repository.
type expectedFile struct {
	// CollectorID, when set, restricts this fixture to one collector. It lets one
	// directory hold every fixture for a vendor.
	CollectorID string `json:"collectorId,omitempty"`
	// Note carries human context: provenance, why this fixture exists, whether it is
	// synthetic. It is preserved across UPDATE_FIXTURES rewrites.
	Note string `json:"note,omitempty"`
	// Error, when set, asserts that Extract fails with an error containing this text.
	Error string `json:"error,omitempty"`
	// WarningsContain asserts that the collector explained a skipped candidate.
	// Preserved across UPDATE_FIXTURES rewrites, because it is an assertion a human
	// wrote on purpose, not a recorded value.
	WarningsContain []string            `json:"warningsContain,omitempty"`
	Candidates      []expectedCandidate `json:"candidates"`
}

type expectedCandidate struct {
	RawVersion        string                `json:"rawVersion"`
	NormalizedVersion string                `json:"normalizedVersion"`
	ReleaseDate       expectedDate          `json:"releaseDate"`
	ReleaseType       string                `json:"releaseType"`
	Channel           string                `json:"channel"`
	Applicability     expectedApplicability `json:"applicability"`

	// Loose assertions; omitted when not asserted.
	EvidenceExcerptContains string  `json:"evidenceExcerptContains,omitempty"`
	ConfidenceAtLeast       float64 `json:"confidenceAtLeast,omitempty"`
	// DedupeKey is compared exactly only when present. The harness always asserts
	// that a dedupe key was computed, because a candidate without one cannot be
	// stored idempotently.
	DedupeKey string `json:"dedupeKey,omitempty"`
}

// expectedDate renders a PartialDate the way the API renders it: at exactly the
// precision the source supported, with the value omitted entirely when unknown. A
// contributor writing a fixture is practising the same discipline the public API
// applies, in the same notation.
type expectedDate struct {
	Precision string `json:"precision"`
	Value     string `json:"value,omitempty"`
}

type expectedApplicability struct {
	ProductHint      string `json:"productHint"`
	HardwareRevision string `json:"hardwareRevision,omitempty"`
	Region           string `json:"region,omitempty"`
	DeploymentMode   string `json:"deploymentMode,omitempty"`
}

func loadExpected(path string) (expectedFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return expectedFile{}, fmt.Errorf("read expected file: %w", err)
	}
	var exp expectedFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&exp); err != nil {
		return expectedFile{}, fmt.Errorf("parse expected file: %w", err)
	}
	return exp, nil
}

// writeExpected rewrites an expected file from observed output, preserving the fields
// a human authored: the collector id, the note and the warning assertions.
func writeExpected(path string, prev expectedFile, got []domain.CandidateRelease) error {
	out := expectedFile{
		CollectorID:     prev.CollectorID,
		Note:            prev.Note,
		Error:           prev.Error,
		WarningsContain: prev.WarningsContain,
		Candidates:      make([]expectedCandidate, 0, len(got)),
	}
	for _, c := range got {
		out.Candidates = append(out.Candidates, observed(c))
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(buf, '\n'), 0o644)
}

// observed projects a candidate onto the stable fields the fixture format compares.
// Generated ids, timestamps and state are deliberately absent: they are assigned by
// the application layer, not by extraction, and comparing them would make every
// fixture a test of the harness rather than of the collector.
func observed(c domain.CandidateRelease) expectedCandidate {
	e := expectedCandidate{
		RawVersion:        c.Version.Raw(),
		NormalizedVersion: c.Version.Normalized(),
		ReleaseDate: expectedDate{
			Precision: string(c.ReleaseDate.Precision()),
			Value:     c.ReleaseDate.String(),
		},
		ReleaseType: string(c.ReleaseType),
		Channel:     c.Applicability.Channel,
		Applicability: expectedApplicability{
			ProductHint:      c.ProductMatchHint,
			HardwareRevision: c.Applicability.HardwareRevision,
			Region:           c.Applicability.Region,
			DeploymentMode:   c.Applicability.DeploymentMode,
		},
	}
	if excerpt := sdk.ExcerptOf(c); excerpt != "" {
		e.EvidenceExcerptContains = firstLineOf(excerpt, 60)
	}
	if c.Confidence > 0 {
		e.ConfidenceAtLeast = c.Confidence
	}
	return e
}

// ---------------------------------------------------------------------------
// Comparison and diff rendering
// ---------------------------------------------------------------------------

func compare(want []expectedCandidate, got []domain.CandidateRelease) string {
	var b strings.Builder

	if len(want) != len(got) {
		fmt.Fprintf(&b, "candidate count: want %d, got %d\n", len(want), len(got))
	}

	n := len(want)
	if len(got) > n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		switch {
		case i >= len(want):
			fmt.Fprintf(&b, "[%d] unexpected candidate: %s\n", i, render(observed(got[i])))
		case i >= len(got):
			fmt.Fprintf(&b, "[%d] missing candidate:    %s\n", i, render(want[i]))
		default:
			if d := compareOne(want[i], got[i]); d != "" {
				fmt.Fprintf(&b, "[%d]\n%s", i, d)
			}
		}
	}
	return b.String()
}

func compareOne(want expectedCandidate, got domain.CandidateRelease) string {
	var b strings.Builder
	mismatch := func(field, w, g string) {
		fmt.Fprintf(&b, "      %-22s want %q\n      %-22s got  %q\n", field+":", w, "", g)
	}

	if want.RawVersion != got.Version.Raw() {
		mismatch("rawVersion", want.RawVersion, got.Version.Raw())
	}
	if want.NormalizedVersion != got.Version.Normalized() {
		mismatch("normalizedVersion", want.NormalizedVersion, got.Version.Normalized())
	}
	if want.ReleaseDate.Precision != string(got.ReleaseDate.Precision()) {
		mismatch("releaseDate.precision", want.ReleaseDate.Precision, string(got.ReleaseDate.Precision()))
	}
	if want.ReleaseDate.Value != got.ReleaseDate.String() {
		mismatch("releaseDate.value", want.ReleaseDate.Value, got.ReleaseDate.String())
	}
	if want.ReleaseType != string(got.ReleaseType) {
		mismatch("releaseType", want.ReleaseType, string(got.ReleaseType))
	}
	if want.Channel != got.Applicability.Channel {
		mismatch("channel", want.Channel, got.Applicability.Channel)
	}
	if want.Applicability.ProductHint != got.ProductMatchHint {
		mismatch("applicability.productHint", want.Applicability.ProductHint, got.ProductMatchHint)
	}
	if want.Applicability.HardwareRevision != got.Applicability.HardwareRevision {
		mismatch("applicability.hardwareRevision", want.Applicability.HardwareRevision, got.Applicability.HardwareRevision)
	}
	if want.Applicability.Region != got.Applicability.Region {
		mismatch("applicability.region", want.Applicability.Region, got.Applicability.Region)
	}
	if want.Applicability.DeploymentMode != got.Applicability.DeploymentMode {
		mismatch("applicability.deploymentMode", want.Applicability.DeploymentMode, got.Applicability.DeploymentMode)
	}

	// Invariant, not an assertion a fixture opts into: a candidate with no dedupe key
	// cannot be stored idempotently, so every run of every collector would duplicate
	// it.
	if strings.TrimSpace(got.DedupeKey) == "" {
		b.WriteString("      dedupeKey:              want a computed key, got \"\"\n")
	}
	if want.DedupeKey != "" && want.DedupeKey != got.DedupeKey {
		mismatch("dedupeKey", want.DedupeKey, got.DedupeKey)
	}

	if want.EvidenceExcerptContains != "" && !strings.Contains(sdk.ExcerptOf(got), want.EvidenceExcerptContains) {
		mismatch("evidence excerpt contains", want.EvidenceExcerptContains, sdk.ExcerptOf(got))
	}
	if want.ConfidenceAtLeast > 0 && got.Confidence < want.ConfidenceAtLeast {
		mismatch("confidence (at least)",
			fmt.Sprintf("%.3f", want.ConfidenceAtLeast), fmt.Sprintf("%.3f", got.Confidence))
	}
	return b.String()
}

func render(c expectedCandidate) string {
	date := c.ReleaseDate.Value
	if date == "" {
		date = "(" + c.ReleaseDate.Precision + ")"
	}
	return fmt.Sprintf("version=%q channel=%q type=%q date=%s hint=%q",
		c.RawVersion, c.Channel, c.ReleaseType, date, c.Applicability.ProductHint)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fixtureName strips the ".fixture.<ext>" suffix: "changelogs.fixture.html" -> "changelogs".
func fixtureName(path string) string {
	base := filepath.Base(path)
	if i := strings.Index(base, ".fixture."); i >= 0 {
		return base[:i]
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".txt":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// syntheticSource builds the minimal source a collector needs to be routed a fixture.
// It is deliberately not a realistic source: extraction reads the collector id and
// nothing else, and a harness that invented scheduling state or compliance status
// would be asserting things extraction has no business seeing.
func syntheticSource(c sdk.Collector, fixturePath string) domain.Source {
	return domain.Source{
		ID:          "src_fixture",
		Slug:        fixtureName(fixturePath),
		CollectorID: c.ID(),
		URL:         "fixture://" + filepath.Base(fixturePath),
		SourceType:  sourceTypeFor(fixturePath),
	}
}

func sourceTypeFor(path string) domain.SourceType {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return domain.SourceTypeHTMLPage
	case ".json":
		return domain.SourceTypeJSONEndpoint
	case ".xml":
		return domain.SourceTypeXMLFeed
	default:
		return domain.SourceTypeRESTAPI
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

func renderWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return "no warnings"
	}
	return "\n  - " + strings.Join(warnings, "\n  - ")
}

func firstLineOf(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = strings.TrimSpace(s[:max])
	}
	return s
}
