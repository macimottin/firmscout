package application_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// ---------------------------------------------------------------------------
// Fakes this file needs that apptest does not provide.
//
// fakeVendors already exists in review_queries_test.go, in this same test package,
// so it is reused rather than declared twice.
// ---------------------------------------------------------------------------

// fakeRegistryLoader hands the sync a fixed set of parsed documents. The point of the
// port is that the use case never learns what YAML is, so a test does not need any.
type fakeRegistryLoader struct {
	docs []application.RegistryDocument
	err  error
}

func (f *fakeRegistryLoader) Load(context.Context) ([]application.RegistryDocument, error) {
	return f.docs, f.err
}

// fakeCategories is a minimal CategoryRepository. The sync accepts a nil one, but a
// device document names categories, so the tests here wire a real vocabulary.
type fakeCategories struct {
	bySlug map[string]domain.Category
}

func newFakeCategories() *fakeCategories {
	return &fakeCategories{bySlug: map[string]domain.Category{}}
}

func (f *fakeCategories) GetBySlug(_ context.Context, slug string) (domain.Category, error) {
	c, ok := f.bySlug[slug]
	if !ok {
		return domain.Category{}, domain.ErrNotFound
	}
	return c, nil
}

func (f *fakeCategories) List(context.Context) ([]domain.Category, error) {
	out := make([]domain.Category, 0, len(f.bySlug))
	for _, c := range f.bySlug {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeCategories) Upsert(_ context.Context, c domain.Category) error {
	f.bySlug[c.Slug] = c
	return nil
}

// fakeRefresher counts the products a sync made readable, in the order it did so. It is
// the whole reason SummaryRefresher is a one-method port: the assertion this file needs
// is "which ids were refreshed", and a counter is the smallest thing that answers it.
type fakeRefresher struct {
	ids []string
	err error
}

func (f *fakeRefresher) RefreshProductSummary(_ context.Context, productID string) error {
	if f.err != nil {
		return f.err
	}
	f.ids = append(f.ids, productID)
	return nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func vendorDoc() application.RegistryDocument {
	return application.RegistryDocument{
		Path:   "dataset/vendors/mikrotik.yaml",
		Vendor: &domain.Vendor{Slug: "mikrotik", Name: "MikroTik"},
	}
}

func osDoc() application.RegistryDocument {
	return application.RegistryDocument{
		Path: "dataset/products/mikrotik/routeros.yaml",
		Product: &domain.Product{
			VendorID:           "mikrotik",
			Slug:               "mikrotik-routeros",
			Name:               "RouterOS",
			DefaultReleaseType: domain.ReleaseTypeEmbeddedOS,
			LifecycleStatus:    domain.LifecycleActive,
		},
	}
}

// deviceDoc is the shape of a registered hardware model: a model identifier, no release
// type of its own, and one runs_os edge whose target is still a registry SLUG.
func deviceDoc(runs ...string) application.RegistryDocument {
	d := application.RegistryDocument{
		Path: "dataset/products/mikrotik/devices/crs328-24p-4s-rm.yaml",
		Product: &domain.Product{
			VendorID:        "mikrotik",
			Slug:            "mikrotik-crs328-24p-4s-rm",
			Name:            "CRS328-24P-4S+RM",
			ModelIdentifier: "CRS328-24P-4S+RM",
			LifecycleStatus: domain.LifecycleUnknown,
		},
	}
	for _, slug := range runs {
		d.Relationships = append(d.Relationships, domain.ProductRelationship{
			ToProductID: slug,
			Kind:        domain.RelationRunsOS,
			SourceNote:  "https://mikrotik.com/product/crs328_24p_4s_rm: Operating System \"RouterOS / SwitchOS\".",
		})
	}
	return d
}

type syncFixture struct {
	uc        *application.SyncRegistry
	products  *apptest.Products
	refresher *fakeRefresher
}

func newSyncFixture(t *testing.T, docs []application.RegistryDocument, withRefresher bool) *syncFixture {
	t.Helper()
	f := &syncFixture{products: apptest.NewProducts(), refresher: &fakeRefresher{}}
	f.uc = application.NewSyncRegistry(
		&fakeRegistryLoader{docs: docs},
		newFakeVendors(),
		newFakeCategories(),
		f.products,
		apptest.NewSources(),
		apptest.NewIDGen(),
		apptest.NewClock(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)),
	)
	if withRefresher {
		f.uc = f.uc.WithSummaries(f.refresher)
	}
	return f
}

// ---------------------------------------------------------------------------
// D6: a synced product that never gets a summary row is a 404 and invisible to search
// ---------------------------------------------------------------------------

func TestSyncRefreshesEverySyncedProductSummary(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, []application.RegistryDocument{
		vendorDoc(), osDoc(), deviceDoc("mikrotik-routeros"),
	}, true)

	report, err := f.uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if report.ProductsUpserted != 2 {
		t.Fatalf("ProductsUpserted = %d, want 2", report.ProductsUpserted)
	}
	if report.SummariesRefreshed != 2 {
		t.Errorf("SummariesRefreshed = %d, want 2", report.SummariesRefreshed)
	}
	if len(f.refresher.ids) != 2 {
		t.Fatalf("refreshed %d product(s), want 2: %v", len(f.refresher.ids), f.refresher.ids)
	}
	// Every id refreshed must be a product this run actually upserted, resolved to a
	// real id rather than to the slug the document carried.
	for _, id := range f.refresher.ids {
		if _, err := f.products.GetByID(context.Background(), id); err != nil {
			t.Errorf("refreshed %q, which is not a product this sync wrote: %v", id, err)
		}
	}
	// The order is deterministic, so two runs over the same registry do the same work
	// in the same sequence rather than in map order.
	if f.refresher.ids[0] > f.refresher.ids[1] {
		t.Errorf("ids were not refreshed in sorted order: %v", f.refresher.ids)
	}
	for _, w := range report.Warnings {
		if strings.Contains(w, "summaries were not refreshed") {
			t.Errorf("a sync that refreshed everything still warned: %q", w)
		}
	}
}

// A sync that upserts products and refreshes nothing has produced a catalogue the
// public API answers 404 for. It is allowed -- the refresher is optional -- but it must
// say so, because the failure is otherwise invisible until somebody curls the site.
func TestSyncWarnsWhenSummariesCannotBeRefreshed(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, []application.RegistryDocument{
		vendorDoc(), osDoc(), deviceDoc("mikrotik-routeros"),
	}, false)

	report, err := f.uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if report.ProductsUpserted != 2 {
		t.Fatalf("ProductsUpserted = %d, want 2", report.ProductsUpserted)
	}
	if report.SummariesRefreshed != 0 {
		t.Errorf("SummariesRefreshed = %d with no refresher attached, want 0", report.SummariesRefreshed)
	}
	var warned string
	for _, w := range report.Warnings {
		if strings.Contains(w, "product summaries were not refreshed") {
			warned = w
		}
	}
	if warned == "" {
		t.Fatalf("no warning named the consequence; warnings: %v", report.Warnings)
	}
	// The warning has to name the consequence, not merely the omission: "2 products
	// were not refreshed" is a fact, "2 products will not be served" is actionable.
	if !strings.Contains(warned, "will not be served by the public API") {
		t.Errorf("warning does not name the consequence: %q", warned)
	}
	if !strings.Contains(warned, "2 product(s)") {
		t.Errorf("warning does not count the affected products: %q", warned)
	}
}

// ---------------------------------------------------------------------------
// The relationships pass
// ---------------------------------------------------------------------------

func TestSyncResolvesRunsSlugToProductID(t *testing.T) {
	t.Parallel()
	// The device document is listed BEFORE the operating system it names, so a sync
	// that resolved the target during the product pass would fail here. File order
	// must not decide whether a registry syncs.
	f := newSyncFixture(t, []application.RegistryDocument{
		vendorDoc(), deviceDoc("mikrotik-routeros"), osDoc(),
	}, true)

	report, err := f.uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if report.RelationshipsUpserted != 1 {
		t.Fatalf("RelationshipsUpserted = %d, want 1", report.RelationshipsUpserted)
	}

	ctx := context.Background()
	device, err := f.products.GetBySlug(ctx, "mikrotik-crs328-24p-4s-rm")
	if err != nil {
		t.Fatalf("device was not upserted: %v", err)
	}
	os, err := f.products.GetBySlug(ctx, "mikrotik-routeros")
	if err != nil {
		t.Fatalf("operating system was not upserted: %v", err)
	}
	rels, err := f.products.ListRelationships(ctx, device.ID)
	if err != nil {
		t.Fatalf("ListRelationships: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("got %d relationship(s), want 1", len(rels))
	}
	got := rels[0]
	if got.FromProductID != device.ID {
		t.Errorf("FromProductID = %q, want the device id %q", got.FromProductID, device.ID)
	}
	if got.ToProductID != os.ID {
		t.Errorf("ToProductID = %q, want the operating system's id %q -- the slug was not resolved", got.ToProductID, os.ID)
	}
	if got.Kind != domain.RelationRunsOS {
		t.Errorf("Kind = %q, want %q", got.Kind, domain.RelationRunsOS)
	}
	if got.ID == "" {
		t.Error("no id was minted for the relationship")
	}
	if got.ManagedBy != domain.ManagedByRegistry {
		t.Errorf("ManagedBy = %q, want %q", got.ManagedBy, domain.ManagedByRegistry)
	}
	if got.RegistryPath != "dataset/products/mikrotik/devices/crs328-24p-4s-rm.yaml" {
		t.Errorf("RegistryPath = %q, want the device's own file", got.RegistryPath)
	}
	// The provenance a reviewer would look for survives the sync unchanged.
	if !strings.Contains(got.SourceNote, "mikrotik.com/product/crs328_24p_4s_rm") {
		t.Errorf("SourceNote lost its provenance: %q", got.SourceNote)
	}
}

// A device naming an operating system nobody registered is a contributor error, and the
// message has to name the file they can open. A foreign-key violation from the driver
// names a constraint instead.
func TestSyncRejectsARunsTargetThatIsNotInTheRegistry(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, []application.RegistryDocument{
		vendorDoc(), osDoc(), deviceDoc("mikrotik-switchos"),
	}, true)

	_, err := f.uc.Execute(context.Background())
	if err == nil {
		t.Fatal("a relationship naming an absent product was accepted")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("error = %v, want it to wrap domain.ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "devices/crs328-24p-4s-rm.yaml") {
		t.Errorf("error does not name the file a contributor can open: %v", err)
	}
	if !strings.Contains(err.Error(), "mikrotik-switchos") {
		t.Errorf("error does not name the unresolvable target: %v", err)
	}
}

// resync applies a second registry over the repository the fixture already populated,
// which is how a contributor editing a file and re-running the sync is reproduced.
func (f *syncFixture) resync(t *testing.T, docs []application.RegistryDocument) application.SyncReport {
	t.Helper()
	second := application.NewSyncRegistry(
		&fakeRegistryLoader{docs: docs},
		newFakeVendors(),
		newFakeCategories(),
		f.products,
		apptest.NewSources(),
		apptest.NewIDGen(),
		apptest.NewClock(time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)),
	).WithSummaries(&fakeRefresher{})
	report, err := second.Execute(context.Background())
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	return report
}

// Deleting a `runs:` block retracts the edge.
//
// This is the whole point of a port called ReplaceRelationships, and it did not hold:
// the pass skipped any document declaring no edge, so an edge could be added by a file
// and never removed by one. The consequence was not abstract -- the device page went on
// naming an operating system the reviewed file had stopped saying the chassis runs,
// which is an unretractable claim about somebody's hardware.
func TestSyncRetractsRelationshipsWhenADocumentDeclaresNone(t *testing.T) {
	t.Parallel()
	docs := []application.RegistryDocument{vendorDoc(), osDoc(), deviceDoc("mikrotik-routeros")}
	f := newSyncFixture(t, docs, true)
	ctx := context.Background()
	if _, err := f.uc.Execute(ctx); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	device, err := f.products.GetBySlug(ctx, "mikrotik-crs328-24p-4s-rm")
	if err != nil {
		t.Fatalf("device was not upserted: %v", err)
	}
	if rels, err := f.products.ListRelationships(ctx, device.ID); err != nil || len(rels) != 1 {
		t.Fatalf("ListRelationships after the first sync = %v, %v; want exactly one edge to retract", rels, err)
	}

	// The same registry, with the runs edge removed from the device's file.
	report := f.resync(t, []application.RegistryDocument{vendorDoc(), osDoc(), deviceDoc()})
	if report.RelationshipsUpserted != 0 {
		t.Errorf("RelationshipsUpserted = %d on a registry declaring none, want 0", report.RelationshipsUpserted)
	}
	rels, err := f.products.ListRelationships(ctx, device.ID)
	if err != nil {
		t.Fatalf("ListRelationships: %v", err)
	}
	if len(rels) != 0 {
		t.Fatalf("got %d relationship(s) after the runs block was deleted, want 0: the edge was not retracted", len(rels))
	}
}

// The alias pass retracts on the same rule, because it had the same bug and because two
// registry passes disagreeing about what "replace" means is worse than either answer.
//
// An alias that cannot be withdrawn is a search hit that cannot be withdrawn: the
// product stays findable under a string the reviewed file no longer claims for it, and
// ADR-0024 makes model-number search run entirely through aliases.
func TestSyncRetractsAliasesWhenADocumentDeclaresNone(t *testing.T) {
	t.Parallel()
	withAlias := deviceDoc("mikrotik-routeros")
	alias, err := domain.NewProductAlias("", "", "CRS328-24P-4S+RM", domain.AliasModelNumber)
	if err != nil {
		t.Fatalf("NewProductAlias: %v", err)
	}
	withAlias.Aliases = []domain.ProductAlias{alias}

	f := newSyncFixture(t, []application.RegistryDocument{vendorDoc(), osDoc(), withAlias}, true)
	ctx := context.Background()
	if _, err := f.uc.Execute(ctx); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	device, err := f.products.GetBySlug(ctx, "mikrotik-crs328-24p-4s-rm")
	if err != nil {
		t.Fatalf("device was not upserted: %v", err)
	}
	if got, err := f.products.ListAliases(ctx, device.ID); err != nil || len(got) != 1 {
		t.Fatalf("ListAliases after the first sync = %v, %v; want exactly one alias to retract", got, err)
	}

	report := f.resync(t, []application.RegistryDocument{vendorDoc(), osDoc(), deviceDoc("mikrotik-routeros")})
	if report.AliasesUpserted != 0 {
		t.Errorf("AliasesUpserted = %d on a registry declaring none, want 0", report.AliasesUpserted)
	}
	got, err := f.products.ListAliases(ctx, device.ID)
	if err != nil {
		t.Fatalf("ListAliases: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d alias(es) after the aliases block was deleted, want 0: the alias was not retracted", len(got))
	}
}

// A document declaring an edge without a product has nowhere to hang it. Rejecting it
// names the file; silently skipping it would drop a fact a contributor wrote down.
func TestSyncRejectsRelationshipsWithoutAProduct(t *testing.T) {
	t.Parallel()
	orphan := application.RegistryDocument{
		Path: "dataset/products/mikrotik/orphan.yaml",
		Relationships: []domain.ProductRelationship{
			{ToProductID: "mikrotik-routeros", Kind: domain.RelationRunsOS},
		},
	}
	f := newSyncFixture(t, []application.RegistryDocument{vendorDoc(), osDoc(), orphan}, true)

	_, err := f.uc.Execute(context.Background())
	if err == nil {
		t.Fatal("a document declaring relationships without a product was accepted")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("error = %v, want it to wrap domain.ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "orphan.yaml") {
		t.Errorf("error does not name the file: %v", err)
	}
}

// A refresher that fails must fail the sync rather than be reported as success with a
// silently unreadable catalogue.
func TestSyncFailsWhenASummaryRefreshFails(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, []application.RegistryDocument{vendorDoc(), osDoc()}, true)
	f.refresher.err = fmt.Errorf("connection reset")
	f.uc = f.uc.WithSummaries(f.refresher)

	if _, err := f.uc.Execute(context.Background()); err == nil {
		t.Fatal("a failing refresher was reported as a successful sync")
	}
}
