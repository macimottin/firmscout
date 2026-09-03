package collectors_test

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/domain"
)

// These tests are the cross-file consistency checks that a JSON Schema cannot express:
// a schema validates one document at a time, and every failure below is a disagreement
// between two documents. They are cheap, offline, and they fail at the moment somebody
// writes the inconsistency rather than at the first scheduled check of a source nobody
// is watching.

type registryDoc struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   map[string]any `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
}

func loadRegistryDocs(t *testing.T, dir string) map[string]registryDoc {
	t.Helper()
	out := make(map[string]registryDoc)
	root := os.DirFS(repoRoot)
	err := fs.WalkDir(root, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		var doc registryDoc
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		out[p] = doc
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no registry documents under %s", dir)
	}
	return out
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// TestDatasetDocumentsAreWellFormed checks the shape every registry document shares.
func TestDatasetDocumentsAreWellFormed(t *testing.T) {
	for dir, wantKind := range map[string]string{
		"dataset/vendors":  "Vendor",
		"dataset/products": "Product",
		"dataset/sources":  "Source",
	} {
		for p, doc := range loadRegistryDocs(t, dir) {
			if doc.APIVersion != collectors.APIVersionV1Alpha1 {
				t.Errorf("%s: apiVersion %q, want %q", p, doc.APIVersion, collectors.APIVersionV1Alpha1)
			}
			if doc.Kind != wantKind {
				t.Errorf("%s: kind %q, want %q", p, doc.Kind, wantKind)
			}
			slug := str(doc.Metadata, "slug")
			if !domain.ValidSlug(slug) {
				t.Errorf("%s: slug %q is not a lowercase kebab-case identifier", p, slug)
			}
		}
	}
}

// TestSourcesUseTheDeclaredVocabularies keeps the registry from carrying values the
// domain does not recognise -- a source_type or quality_class nobody implemented is a
// row that silently never dispatches.
func TestSourcesUseTheDeclaredVocabularies(t *testing.T) {
	for p, doc := range loadRegistryDocs(t, "dataset/sources") {
		if !domain.ValidSourceType(domain.SourceType(str(doc.Spec, "source_type"))) {
			t.Errorf("%s: source_type %q is not a known source type", p, str(doc.Spec, "source_type"))
		}
		if !domain.ValidQualityClass(domain.QualityClass(str(doc.Spec, "quality_class"))) {
			t.Errorf("%s: quality_class %q is not a known quality class", p, str(doc.Spec, "quality_class"))
		}
		if !domain.ValidSourceHealth(domain.SourceHealth(str(doc.Spec, "health"))) {
			t.Errorf("%s: health %q is not a known health state", p, str(doc.Spec, "health"))
		}
		url := str(doc.Spec, "url")
		if !strings.HasPrefix(url, "https://") {
			t.Errorf("%s: url %q must be https", p, url)
		}
	}
}

// TestNoSourceIsEnabledWithoutATermsReview is ADR-0018 expressed as a test.
//
// It deliberately does not assert "enabled is false". Asserting that would break the
// day a human legitimately completes the terms review and turns a source on, which is
// the workflow this rule exists to protect, not to prevent. What it asserts is the
// rule itself: collection is enabled only where robots permits it and a human has
// reviewed the terms.
func TestNoSourceIsEnabledWithoutATermsReview(t *testing.T) {
	for p, doc := range loadRegistryDocs(t, "dataset/sources") {
		enabled, _ := doc.Spec["enabled"].(bool)
		if !enabled {
			continue
		}
		src := domain.Source{
			RobotsPolicyStatus: domain.RobotsPolicyStatus(str(doc.Spec, "robots_policy_status")),
			TermsReviewStatus:  domain.TermsReviewStatus(str(doc.Spec, "terms_review_status")),
		}
		if !src.CompliancePermitsCollection() {
			t.Errorf("%s is enabled but its compliance status forbids collection "+
				"(robots=%q, terms=%q); ADR-0018 requires a human terms review before a source is enabled",
				p, src.RobotsPolicyStatus, src.TermsReviewStatus)
		}
	}
}

// TestMikroTikPilotSourcesShipDisabled records the state the two pilot sources are in
// today, and why. If someone enables them, this test tells them exactly which other
// facts must move at the same time.
func TestMikroTikPilotSourcesShipDisabled(t *testing.T) {
	docs := loadRegistryDocs(t, "dataset/sources")
	for _, p := range []string{"dataset/sources/mikrotik/changelogs.yaml", "dataset/sources/mikrotik/newest-stable.yaml"} {
		doc, ok := docs[p]
		if !ok {
			t.Fatalf("%s is missing", p)
		}
		if enabled, _ := doc.Spec["enabled"].(bool); enabled {
			t.Errorf("%s is enabled; the ADR-0018 terms review for mikrotik.com has not been done. "+
				"If it has been done, update terms_review_status here and remove this expectation.", p)
		}
		if got := str(doc.Spec, "terms_review_status"); got != string(domain.TermsPending) {
			t.Errorf("%s: terms_review_status is %q, expected %q", p, got, domain.TermsPending)
		}
		// robots.txt was measured on 2026-09-03: "User-agent: *" with an empty
		// "Disallow:". That is a mechanical fact and it is recorded as one.
		if got := str(doc.Spec, "robots_policy_status"); got != string(domain.RobotsAllowed) {
			t.Errorf("%s: robots_policy_status is %q, expected %q (measured 2026-09-03)", p, got, domain.RobotsAllowed)
		}
	}
}

// TestEverySourceResolvesToARegisteredCollector is the cross-file check the spec calls
// for: a source naming a collector that does not exist would fail at dispatch time,
// long after the pull request that introduced it was merged.
func TestEverySourceResolvesToARegisteredCollector(t *testing.T) {
	r := newShippedRegistry(t)
	for p, doc := range loadRegistryDocs(t, "dataset/sources") {
		id := str(doc.Spec, "collector_id")
		if id == "" {
			continue // a manually maintained source legitimately has no collector
		}
		if _, err := r.For(domain.Source{ID: p, Slug: str(doc.Metadata, "slug"), CollectorID: id}); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// TestEveryCollectorConfigReferencesRealRegistryEntries is the reverse direction:
// metadata.vendor and product_match.product must name entries that exist.
func TestEveryCollectorConfigReferencesRealRegistryEntries(t *testing.T) {
	vendors := make(map[string]bool)
	for _, doc := range loadRegistryDocs(t, "dataset/vendors") {
		vendors[str(doc.Metadata, "slug")] = true
	}
	products := make(map[string]bool)
	for _, doc := range loadRegistryDocs(t, "dataset/products") {
		products[str(doc.Metadata, "slug")] = true
	}

	configs, err := collectors.LoadDir(os.DirFS(repoRoot), configRoot)
	if err != nil {
		t.Fatalf("load configs: %v", err)
	}
	for _, cfg := range configs {
		if !vendors[cfg.Metadata.Vendor] {
			t.Errorf("%s: metadata.vendor %q has no dataset/vendors entry", cfg.Path, cfg.Metadata.Vendor)
		}
		if p := cfg.Spec.ProductMatch.Product; p != "" && !products[p] {
			t.Errorf("%s: spec.product_match.product %q has no dataset/products entry", cfg.Path, p)
		}
	}
}

// TestSourceAndCollectorConfigAgree catches the two documents drifting apart: the
// source record is what the scheduler and fetcher read, the collector config is what
// the engine reads, and a disagreement between them is invisible until it matters.
func TestSourceAndCollectorConfigAgree(t *testing.T) {
	configs, err := collectors.LoadDir(os.DirFS(repoRoot), configRoot)
	if err != nil {
		t.Fatalf("load configs: %v", err)
	}
	byID := make(map[string]collectors.Config, len(configs))
	for _, cfg := range configs {
		byID[cfg.Metadata.ID] = cfg
	}

	for p, doc := range loadRegistryDocs(t, "dataset/sources") {
		cfg, ok := byID[str(doc.Spec, "collector_id")]
		if !ok {
			continue
		}
		if want, got := str(doc.Spec, "expected_content_type"), cfg.Spec.Fetch.ExpectedContentType; want != "" && got != "" && want != got {
			t.Errorf("%s: expected_content_type %q disagrees with %s (%q)", p, want, cfg.Path, got)
		}
		if want, got := str(doc.Metadata, "vendor"), cfg.Metadata.Vendor; want != got {
			t.Errorf("%s: vendor %q disagrees with %s (%q)", p, want, cfg.Path, got)
		}
		if freq, okFreq := doc.Spec["check_frequency_seconds"].(int); okFreq && cfg.Spec.Schedule.CheckFrequencySeconds != 0 {
			if freq != cfg.Spec.Schedule.CheckFrequencySeconds {
				t.Errorf("%s: check_frequency_seconds %d disagrees with %s (%d)",
					p, freq, cfg.Path, cfg.Spec.Schedule.CheckFrequencySeconds)
			}
		}
		// The normalisation section selector is the source's change-detection
		// configuration and the engine's extraction scope. They must be the same
		// string or the engine reads content the hash ignores.
		if norm, okNorm := doc.Spec["normalize"].(map[string]any); okNorm {
			if want, got := str(norm, "section_selector"), cfg.Spec.Normalize.SectionSelector; want != got {
				t.Errorf("%s: normalize.section_selector %q disagrees with %s (%q)", p, want, cfg.Path, got)
			}
		}
	}
}
