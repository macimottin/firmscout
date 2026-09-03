package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/macimottin/firmscout/internal/domain"
)

func TestVendorRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := NewVendorRepo(db)

	v := newVendor("ven_mikrotik", "mikrotik")
	v.LegalName = "Mikrotikls SIA"
	v.CountryCode = "LV"
	if err := repo.Upsert(ctx, v); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetBySlug(ctx, "mikrotik")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ID != v.ID || got.Name != v.Name || got.LegalName != v.LegalName || got.CountryCode != "LV" {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.ManagedBy != domain.ManagedByRegistry {
		t.Errorf("ManagedBy = %q, want registry", got.ManagedBy)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps were not populated by the database")
	}

	// Upsert is an update, not a second insert.
	v.Name = "MikroTik"
	if err := repo.Upsert(ctx, v); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	got, err = repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "MikroTik" {
		t.Errorf("Name after update = %q, want MikroTik", got.Name)
	}

	if _, err := repo.GetBySlug(ctx, "nobody"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetBySlug on a missing vendor = %v, want domain.ErrNotFound", err)
	}
}

func TestVendorSlugCollisionIsConflict(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := NewVendorRepo(db)

	if err := repo.Upsert(ctx, newVendor("ven_a", "shared")); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	err := repo.Upsert(ctx, newVendor("ven_b", "shared"))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second vendor claiming the same slug = %v, want domain.ErrConflict", err)
	}
}

// TestNotFoundDoesNotLeakDriverError pins the package's boundary rule: a missing row
// is domain.ErrNotFound and pgx.ErrNoRows is not reachable through it, so an
// application-layer errors.Is cannot accidentally depend on the driver.
func TestNotFoundDoesNotLeakDriverError(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	_, err := NewVendorRepo(db).GetByID(ctx, "ven_does_not_exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByID on a missing vendor = %v, want domain.ErrNotFound", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("pgx.ErrNoRows escaped the adapter through %v", err)
	}
}

func TestVendorListPaginates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := NewVendorRepo(db)

	for _, slug := range []string{"aaa", "bbb", "ccc", "ddd", "eee"} {
		if err := repo.Upsert(ctx, newVendor("ven_"+slug, slug)); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}

	page1, cursor, err := repo.List(ctx, 2, "")
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(page1) != 2 || page1[0].Slug != "aaa" || page1[1].Slug != "bbb" {
		t.Fatalf("page 1 = %v", slugsOf(page1))
	}
	if cursor == "" {
		t.Fatal("page 1 returned no cursor despite a full page")
	}

	page2, cursor2, err := repo.List(ctx, 2, cursor)
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(page2) != 2 || page2[0].Slug != "ccc" {
		t.Fatalf("page 2 = %v", slugsOf(page2))
	}

	page3, cursor3, err := repo.List(ctx, 2, cursor2)
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(page3) != 1 || page3[0].Slug != "eee" {
		t.Fatalf("page 3 = %v", slugsOf(page3))
	}
	if cursor3 != "" {
		t.Errorf("cursor after a short page = %q, want empty", cursor3)
	}

	if _, _, err := repo.List(ctx, 2, "not-base64!!"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("malformed cursor = %v, want domain.ErrValidation", err)
	}
}

func slugsOf(vs []domain.Vendor) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Slug)
	}
	return out
}

func TestProductAndFamilyRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	vendors := NewVendorRepo(db)
	products := NewProductRepo(db)

	v := newVendor("ven_mikrotik", "mikrotik")
	if err := vendors.Upsert(ctx, v); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}

	fam := domain.ProductFamily{
		ID:       "fam_ccr",
		VendorID: v.ID,
		Slug:     "cloud-core-router",
		Name:     "Cloud Core Router",
	}
	if err := products.UpsertFamily(ctx, fam); err != nil {
		t.Fatalf("UpsertFamily: %v", err)
	}
	gotFam, err := products.GetFamilyBySlug(ctx, v.ID, "cloud-core-router")
	if err != nil {
		t.Fatalf("GetFamilyBySlug: %v", err)
	}
	if gotFam.Name != fam.Name {
		t.Errorf("family name = %q, want %q", gotFam.Name, fam.Name)
	}
	if _, err := products.GetFamilyBySlug(ctx, v.ID, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing family = %v, want domain.ErrNotFound", err)
	}

	p := newProduct("prd_ccr2004", v.ID, "ccr2004")
	p.ProductFamilyID = fam.ID
	p.ModelIdentifier = "CCR2004-1G-12S+2XS"
	p.SecurityCritical = true
	p.PopularityScore = 42
	if err := products.Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert product: %v", err)
	}

	got, err := products.GetBySlug(ctx, "ccr2004")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ProductFamilyID != fam.ID || got.ModelIdentifier != p.ModelIdentifier ||
		!got.SecurityCritical || got.PopularityScore != 42 ||
		got.DefaultReleaseType != domain.ReleaseTypeFirmware ||
		got.LifecycleStatus != domain.LifecycleActive {
		t.Errorf("round trip lost data: %+v", got)
	}
	if len(got.CategorySlugs) != 0 {
		t.Errorf("CategorySlugs = %v, want empty", got.CategorySlugs)
	}

	byVendor, _, err := products.ListByVendor(ctx, v.ID, 10, "")
	if err != nil {
		t.Fatalf("ListByVendor: %v", err)
	}
	if len(byVendor) != 1 || byVendor[0].ID != p.ID {
		t.Errorf("ListByVendor = %d rows, want the one product", len(byVendor))
	}
}

func TestProductUpsertRejectsUnknownCategorySlug(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, p := seedCatalog(t, db)

	p.CategorySlugs = []string{"routers"}
	err := NewProductRepo(db).Upsert(ctx, p)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Upsert with an unknown category = %v, want domain.ErrValidation", err)
	}

	// Once the category exists the same call succeeds and the membership round-trips.
	if _, err := pool(db).Exec(ctx,
		`INSERT INTO categories (id, slug, name) VALUES ('cat_routers', 'routers', 'Routers')`); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	if err := NewProductRepo(db).Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert with a known category: %v", err)
	}
	got, err := NewProductRepo(db).GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.CategorySlugs) != 1 || got.CategorySlugs[0] != "routers" {
		t.Errorf("CategorySlugs = %v, want [routers]", got.CategorySlugs)
	}

	// Removing the slug removes the membership.
	p.CategorySlugs = nil
	if err := NewProductRepo(db).Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert clearing categories: %v", err)
	}
	got, err = NewProductRepo(db).GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.CategorySlugs) != 0 {
		t.Errorf("CategorySlugs after clearing = %v, want empty", got.CategorySlugs)
	}
}

func TestAliasesReplaceAndList(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, p := seedCatalog(t, db)
	products := NewProductRepo(db)

	a1, err := domain.NewProductAlias("pal_1", p.ID, "CCR 2004", domain.AliasMarketingName)
	if err != nil {
		t.Fatalf("build alias: %v", err)
	}
	a2, err := domain.NewProductAlias("pal_2", p.ID, "CCR2004-1G-12S+2XS", domain.AliasModelNumber)
	if err != nil {
		t.Fatalf("build alias: %v", err)
	}
	if err := products.ReplaceAliases(ctx, p.ID, []domain.ProductAlias{a1, a2}); err != nil {
		t.Fatalf("ReplaceAliases: %v", err)
	}

	got, err := products.ListAliases(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListAliases: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("aliases = %d, want 2", len(got))
	}

	// Replacement removes what is no longer in the registry file.
	if err := products.ReplaceAliases(ctx, p.ID, []domain.ProductAlias{a1}); err != nil {
		t.Fatalf("second ReplaceAliases: %v", err)
	}
	got, err = products.ListAliases(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListAliases: %v", err)
	}
	if len(got) != 1 || got[0].Alias != "CCR 2004" {
		t.Errorf("aliases after replacement = %+v, want only the marketing name", got)
	}
}

// TestResolveByAlias covers the three answers the ingestion pipeline must be able to
// tell apart: nothing matched, exactly one thing matched, and several things matched.
// The last is the one that must never be silently collapsed into a guess.
func TestResolveByAlias(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	vendors := NewVendorRepo(db)
	products := NewProductRepo(db)

	v := newVendor("ven_cisco", "cisco")
	if err := vendors.Upsert(ctx, v); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	for _, slug := range []string{"catalyst-9300-24t", "catalyst-9300-48p", "catalyst-9200"} {
		if err := products.Upsert(ctx, newProduct("prd_"+slug, v.ID, slug)); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}

	// A vendor-internal alias shared by two products: genuinely ambiguous.
	for _, id := range []string{"prd_catalyst-9300-24t", "prd_catalyst-9300-48p"} {
		a, err := domain.NewProductAlias("", id, "Catalyst 9300", domain.AliasMarketingName)
		if err != nil {
			t.Fatalf("build alias: %v", err)
		}
		if err := products.ReplaceAliases(ctx, id, []domain.ProductAlias{a}); err != nil {
			t.Fatalf("ReplaceAliases: %v", err)
		}
	}
	// catalyst-9200 gets two aliases: an ordinary one, and one whose normalised form
	// is identical to the product's own slug. The second exists so that a query
	// matching both the slug branch and the alias branch of the predicate is proven to
	// return the product once rather than twice -- a duplicate row here would be
	// indistinguishable from genuine ambiguity.
	shortAlias, err := domain.NewProductAlias("pal_c9200", "prd_catalyst-9200", "C9200", domain.AliasModelNumber)
	if err != nil {
		t.Fatalf("build alias: %v", err)
	}
	slugAlias := domain.ProductAlias{
		ID:              "pal_slugform",
		ProductID:       "prd_catalyst-9200",
		Alias:           "catalyst-9200",
		NormalizedAlias: "catalyst-9200",
		Kind:            domain.AliasLegacyName,
	}
	if err := products.ReplaceAliases(ctx, "prd_catalyst-9200",
		[]domain.ProductAlias{shortAlias, slugAlias}); err != nil {
		t.Fatalf("ReplaceAliases: %v", err)
	}

	t.Run("no match", func(t *testing.T) {
		got, err := products.ResolveByAlias(ctx, v.ID, domain.NormalizeAlias("Nexus 9000"))
		if err != nil {
			t.Fatalf("ResolveByAlias: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("matches = %d, want 0", len(got))
		}
	})

	t.Run("unique match by slug and alias at once", func(t *testing.T) {
		got, err := products.ResolveByAlias(ctx, v.ID, "catalyst-9200")
		if err != nil {
			t.Fatalf("ResolveByAlias: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "catalyst-9200" {
			t.Errorf("matches = %+v, want exactly one row for catalyst-9200", got)
		}
	})

	t.Run("unique match by alias only", func(t *testing.T) {
		got, err := products.ResolveByAlias(ctx, v.ID, domain.NormalizeAlias("C9200"))
		if err != nil {
			t.Fatalf("ResolveByAlias: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "catalyst-9200" {
			t.Errorf("matches = %+v, want exactly catalyst-9200", got)
		}
	})

	t.Run("ambiguous match", func(t *testing.T) {
		got, err := products.ResolveByAlias(ctx, v.ID, domain.NormalizeAlias("Catalyst 9300"))
		if err != nil {
			t.Fatalf("ResolveByAlias: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("matches = %d, want 2 so the caller can detect ambiguity", len(got))
		}
	})

	t.Run("scoped to the vendor", func(t *testing.T) {
		other := newVendor("ven_other", "other")
		if err := vendors.Upsert(ctx, other); err != nil {
			t.Fatalf("seed vendor: %v", err)
		}
		got, err := products.ResolveByAlias(ctx, other.ID, domain.NormalizeAlias("Catalyst 9300"))
		if err != nil {
			t.Fatalf("ResolveByAlias: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("matches for another vendor = %d, want 0", len(got))
		}
	})
}
