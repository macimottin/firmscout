package registry_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/macimottin/firmscout/internal/adapters/registry"
	"github.com/macimottin/firmscout/internal/domain"
)

// The loader is pointed at the repository's own dataset directory. This is the one
// place where testing against the real files is right: they are checked-in data, not a
// live website, and a change that breaks them should break the build.
func TestLoadsTheRepositoryRegistry(t *testing.T) {
	t.Parallel()
	l := registry.New(os.DirFS("../../.."), "dataset")
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no registry documents loaded")
	}

	var vendors, products, sources int
	for _, d := range docs {
		switch {
		case d.Vendor != nil:
			vendors++
		case d.Product != nil:
			products++
		case d.Source != nil:
			sources++
		}
		if d.Path == "" {
			t.Error("a document was loaded without recording its path; an error would name nothing a contributor can open")
		}
	}
	if vendors == 0 || products == 0 || sources == 0 {
		t.Errorf("expected at least one of each kind, got %d vendors, %d products, %d sources", vendors, products, sources)
	}
}

// Every source in the repository ships uncollectable. This is not an accident of the
// current data: ADR-0018 requires a human to review a site's terms before FirmScout
// fetches from it, and a test is the only thing that stops that discipline eroding.
func TestEverySourceShipsPendingTermsReview(t *testing.T) {
	t.Parallel()
	l := registry.New(os.DirFS("../../.."), "dataset")
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, d := range docs {
		if d.Source == nil {
			continue
		}
		s := *d.Source
		if s.CompliancePermitsCollection() && s.Enabled {
			t.Errorf("%s: source %q is enabled and collectable in the committed registry; "+
				"a source must not be collectable until a maintainer has reviewed its terms (ADR-0018)",
				d.Path, s.Slug)
		}
	}
}

func TestRejectsUnsupportedAPIVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "v.yaml", `
apiVersion: firmscout.dev/v99
kind: Vendor
metadata: {slug: acme}
spec: {name: Acme}
`)
	_, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err == nil {
		t.Fatal("an unsupported apiVersion was accepted")
	}
	if !strings.Contains(err.Error(), "apiVersion") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

// A misspelled key that is silently ignored produces a source that looks configured
// and behaves differently, which is worse than a parse failure.
func TestRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "v.yaml", `
apiVersion: firmscout.dev/v1alpha1
kind: Vendor
metadata: {slug: acme}
spec:
  name: Acme
  homepage_ur1: https://acme.example
`)
	_, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err == nil {
		t.Fatal("an unknown field was silently ignored")
	}
}

func TestSourceDefaultsToPendingTermsWhenOmitted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "s.yaml", `
apiVersion: firmscout.dev/v1alpha1
kind: Source
metadata: {slug: releases, vendor: acme}
spec:
  source_type: html_page
  url: https://acme.example/releases
  robots_policy_status: allowed
`)
	docs, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := docs[0].Source
	if s.TermsReviewStatus != domain.TermsPending {
		t.Errorf("terms status = %q, want pending; omitting the field must never mean approved", s.TermsReviewStatus)
	}
	if s.CompliancePermitsCollection() {
		t.Error("a source with unreviewed terms reported itself collectable")
	}
}

// A third-party page marked official would silently outrank the vendor's own.
func TestRejectsOfficialFlagOnAThirdPartySource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "s.yaml", `
apiVersion: firmscout.dev/v1alpha1
kind: Source
metadata: {slug: forum, vendor: acme}
spec:
  source_type: html_page
  url: https://forum.example/acme
  official: true
  quality_class: trusted_community
  robots_policy_status: allowed
  terms_review_status: approved
`)
	_, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err == nil {
		t.Fatal("a community source was accepted as official")
	}
	if !strings.Contains(err.Error(), "official") {
		t.Errorf("error does not explain the conflict: %v", err)
	}
}

func TestRejectsUnknownEnumValues(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, body string }{
		{"source type", `
apiVersion: firmscout.dev/v1alpha1
kind: Source
metadata: {slug: s, vendor: acme}
spec: {source_type: carrier_pigeon, url: "https://acme.example", robots_policy_status: allowed}`},
		{"release type", `
apiVersion: firmscout.dev/v1alpha1
kind: Product
metadata: {slug: acme-thing, vendor: acme}
spec: {name: Thing, default_release_type: magic}`},
		{"lifecycle status", `
apiVersion: firmscout.dev/v1alpha1
kind: Product
metadata: {slug: acme-thing, vendor: acme}
spec: {name: Thing, lifecycle_status: probably_fine}`},
		{"robots status", `
apiVersion: firmscout.dev/v1alpha1
kind: Source
metadata: {slug: s, vendor: acme}
spec: {source_type: html_page, url: "https://acme.example", robots_policy_status: maybe}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, dir, "d.yaml", tc.body)
			if _, err := registry.New(os.DirFS(dir), ".").Load(context.Background()); err == nil {
				t.Fatalf("an invalid %s was accepted", tc.name)
			}
		})
	}
}

func TestProductAliasesAreNormalised(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.yaml", `
apiVersion: firmscout.dev/v1alpha1
kind: Product
metadata: {slug: acme-rb5009, vendor: acme}
spec:
  name: RB5009
  aliases:
    - alias: "RB5009UG+S+IN"
      kind: model_number
`)
	docs, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs[0].Aliases) != 1 {
		t.Fatalf("got %d aliases, want 1", len(docs[0].Aliases))
	}
	a := docs[0].Aliases[0]
	if a.Alias != "RB5009UG+S+IN" {
		t.Errorf("the original alias text was not preserved: %q", a.Alias)
	}
	if a.NormalizedAlias != "rb5009ug s in" {
		t.Errorf("normalised alias = %q, want %q", a.NormalizedAlias, "rb5009ug s in")
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(strings.TrimLeft(body, "\n")), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
