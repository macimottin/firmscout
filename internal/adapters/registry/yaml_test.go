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

	var vendors, families, products, sources int
	for _, d := range docs {
		switch {
		case d.Vendor != nil:
			vendors++
		case d.Family != nil:
			families++
		case d.Product != nil:
			products++
		case d.Source != nil:
			sources++
		}
		if d.Path == "" {
			t.Error("a document was loaded without recording its path; an error would name nothing a contributor can open")
		}
	}
	if vendors == 0 || families == 0 || products == 0 || sources == 0 {
		t.Errorf("expected at least one of each kind, got %d vendors, %d families, %d products, %d sources",
			vendors, families, products, sources)
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

// A fleet manager holds model numbers, not product names, so a device that loses its
// model_number alias becomes unreachable by the only string its owner has. Nothing else
// in the system would notice: the product still resolves by slug, still renders a page
// and still appears in vendor listings, and only a search for the code stamped on the
// chassis comes back empty. This test is the thing that notices. See ADR-0024.
func TestEveryDeviceCarriesAModelNumberAlias(t *testing.T) {
	t.Parallel()
	l := registry.New(os.DirFS("../../.."), "dataset")
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	devices := 0
	for _, d := range docs {
		if d.Product == nil || !d.Product.IsHardwareModel() {
			continue
		}
		devices++
		found := false
		for _, a := range d.Aliases {
			if a.Kind == domain.AliasModelNumber && a.Alias == d.Product.ModelIdentifier {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: product %q publishes model identifier %q but declares no model_number alias "+
				"with that exact text; a fleet inventory pasting the code would match nothing",
				d.Path, d.Product.Slug, d.Product.ModelIdentifier)
		}
	}
	if devices == 0 {
		t.Fatal("no hardware models in the committed registry; this test would pass vacuously")
	}
}

// A device page whose whole purpose is to say "the firmware is published over there"
// must actually name a there, and the slug it names must resolve. The loader cannot
// check the second half -- it parses one file at a time -- so the dataset-wide check
// lives here rather than in parseProduct.
func TestDeviceDocumentsDeclareTheOSTheyRun(t *testing.T) {
	t.Parallel()
	l := registry.New(os.DirFS("../../.."), "dataset")
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	declared := map[string]bool{}
	for _, d := range docs {
		if d.Product != nil {
			declared[d.Product.Slug] = true
		}
	}

	for _, d := range docs {
		if d.Product == nil || !d.Product.IsHardwareModel() {
			continue
		}
		runsAnOS := false
		for _, r := range d.Relationships {
			if r.Kind == domain.RelationRunsOS {
				runsAnOS = true
			}
			// ToProductID still holds the target's registry slug at this stage; the
			// sync resolves it to an id once every product has been upserted.
			if !declared[r.ToProductID] {
				t.Errorf("%s: product %q runs %q, which no registry document declares",
					d.Path, d.Product.Slug, r.ToProductID)
			}
		}
		if !runsAnOS {
			t.Errorf("%s: hardware model %q declares no runs_os relationship, so its page would "+
				"name no operating system and lead nowhere", d.Path, d.Product.Slug)
		}
	}
}

// The relation vocabulary has one member, and a document inventing a second must fail
// by name rather than be stored as an edge nothing renders.
func TestRejectsUnknownRelationKind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.yaml", `
apiVersion: firmscout.dev/v1alpha1
kind: Product
metadata: {slug: acme-box, vendor: acme}
spec:
  name: Box
  model_identifier: BOX-1
  runs:
    - product: acme-os
      kind: contains
`)
	_, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err == nil {
		t.Fatal("an unknown relation kind was accepted")
	}
	if !strings.Contains(err.Error(), "contains") {
		t.Errorf("error does not name the offending kind: %v", err)
	}
}

// A runs target that is not even slug-shaped is a contributor typo, and catching it at
// parse time names the file it is in. A target that IS slug-shaped but names no product
// is caught later, by the sync, which is the only place that knows every document.
func TestRejectsMalformedRunsTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.yaml", `
apiVersion: firmscout.dev/v1alpha1
kind: Product
metadata: {slug: acme-box, vendor: acme}
spec:
  name: Box
  runs:
    - product: "Acme OS"
`)
	_, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err == nil {
		t.Fatal("a runs target that is not a slug was accepted")
	}
	if !strings.Contains(err.Error(), "runs.product") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// The relation kind is optional in the file because there is exactly one of them, and
// making a contributor spell it out on every device would be ceremony. It must still
// arrive as a real kind rather than as the empty string.
func TestRunsKindDefaultsToRunsOS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.yaml", `
apiVersion: firmscout.dev/v1alpha1
kind: Product
metadata: {slug: acme-box, vendor: acme}
spec:
  name: Box
  runs:
    - product: acme-os
`)
	docs, err := registry.New(os.DirFS(dir), ".").Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs[0].Relationships) != 1 {
		t.Fatalf("got %d relationships, want 1", len(docs[0].Relationships))
	}
	r := docs[0].Relationships[0]
	if r.Kind != domain.RelationRunsOS {
		t.Errorf("relation kind = %q, want %q", r.Kind, domain.RelationRunsOS)
	}
	if r.ToProductID != "acme-os" {
		t.Errorf("target = %q, want the unresolved slug %q", r.ToProductID, "acme-os")
	}
	if r.ManagedBy != domain.ManagedByRegistry {
		t.Errorf("managed_by = %q, want %q", r.ManagedBy, domain.ManagedByRegistry)
	}
}
