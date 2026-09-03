package postgres

import (
	"context"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// VendorRepo is the PostgreSQL implementation of application.VendorRepository.
//
// Vendors are a registry table: rows synchronised from the Git registry carry
// managed_by='registry' and the path they came from, so `firmscout registry sync` can
// tell its own rows from ones an operator created by hand and only overwrite the
// former.
type VendorRepo struct{ db *DB }

var _ application.VendorRepository = (*VendorRepo)(nil)

// NewVendorRepo returns a vendor repository bound to db. It holds no connection of its
// own; each call resolves the pool or the ambient transaction from its context.
func NewVendorRepo(db *DB) *VendorRepo { return &VendorRepo{db: db} }

const vendorColumns = `
    id, slug, name, legal_name, homepage_url, support_url, country_code, notes,
    managed_by, registry_path, created_at, updated_at`

func scanVendor(row interface{ Scan(...any) error }) (domain.Vendor, error) {
	var (
		v            domain.Vendor
		legalName    *string
		homepageURL  *string
		supportURL   *string
		countryCode  *string
		notes        *string
		registryPath *string
		managedBy    string
	)
	if err := row.Scan(
		&v.ID, &v.Slug, &v.Name, &legalName, &homepageURL, &supportURL, &countryCode,
		&notes, &managedBy, &registryPath, &v.CreatedAt, &v.UpdatedAt,
	); err != nil {
		return domain.Vendor{}, err
	}
	v.LegalName = str(legalName)
	v.HomepageURL = str(homepageURL)
	v.SupportURL = str(supportURL)
	v.CountryCode = str(countryCode)
	v.Notes = str(notes)
	v.RegistryPath = str(registryPath)
	v.ManagedBy = domain.ManagedBy(managedBy)
	return v, nil
}

// GetByID returns the vendor with the given id, or domain.ErrNotFound.
func (r *VendorRepo) GetByID(ctx context.Context, id string) (domain.Vendor, error) {
	row := r.db.q(ctx).QueryRow(ctx, `SELECT`+vendorColumns+` FROM vendors WHERE id = $1`, id)
	v, err := scanVendor(row)
	if err != nil {
		return domain.Vendor{}, wrap("vendor.GetByID", err)
	}
	return v, nil
}

// GetBySlug returns the vendor with the given slug, or domain.ErrNotFound. Slugs are
// globally unique and are the identifier the public API exposes, so this is the hot
// path rather than GetByID.
func (r *VendorRepo) GetBySlug(ctx context.Context, slug string) (domain.Vendor, error) {
	row := r.db.q(ctx).QueryRow(ctx, `SELECT`+vendorColumns+` FROM vendors WHERE slug = $1`, slug)
	v, err := scanVendor(row)
	if err != nil {
		return domain.Vendor{}, wrap("vendor.GetBySlug", err)
	}
	return v, nil
}

// List returns a page of vendors ordered by slug, together with the cursor for the
// next page. An empty cursor starts at the beginning; an empty returned cursor means
// the caller has reached the end.
//
// Pagination is keyset rather than offset: vendors are inserted continuously by
// registry sync, and an offset paginator would skip or repeat rows as the table grows
// underneath a client walking it.
func (r *VendorRepo) List(ctx context.Context, limit int, cursor string) ([]domain.Vendor, string, error) {
	limit = clampLimit(limit, 50, 500)
	parts, err := decodeCursor(cursor, 1)
	if err != nil {
		return nil, "", err
	}
	after := ""
	if parts != nil {
		after = parts[0]
	}

	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+vendorColumns+`
         FROM vendors
         WHERE ($1::text = '' OR slug > $1)
         ORDER BY slug
         LIMIT $2`, after, limit)
	if err != nil {
		return nil, "", wrap("vendor.List", err)
	}
	defer rows.Close()

	out := make([]domain.Vendor, 0, limit)
	for rows.Next() {
		v, err := scanVendor(rows)
		if err != nil {
			return nil, "", wrap("vendor.List.scan", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, "", wrap("vendor.List.rows", err)
	}

	next := ""
	if len(out) == limit {
		next = encodeCursor(out[len(out)-1].Slug)
	}
	return out, next, nil
}

// Upsert inserts a vendor or updates the existing row with the same id.
//
// Conflict on slug rather than id -- two different vendor ids claiming one slug -- is
// reported as domain.ErrConflict rather than silently resolved, because which of the
// two is correct is not something persistence can decide.
func (r *VendorRepo) Upsert(ctx context.Context, v domain.Vendor) error {
	if err := v.Validate(); err != nil {
		return err
	}
	managedBy := v.ManagedBy
	if managedBy == "" {
		managedBy = domain.ManagedByRegistry
	}

	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO vendors (
            id, slug, name, legal_name, homepage_url, support_url, country_code, notes,
            managed_by, registry_path, created_at, updated_at
         ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
            COALESCE($11::timestamptz, now()), COALESCE($12::timestamptz, now())
         )
         ON CONFLICT (id) DO UPDATE SET
            slug          = EXCLUDED.slug,
            name          = EXCLUDED.name,
            legal_name    = EXCLUDED.legal_name,
            homepage_url  = EXCLUDED.homepage_url,
            support_url   = EXCLUDED.support_url,
            country_code  = EXCLUDED.country_code,
            notes         = EXCLUDED.notes,
            managed_by    = EXCLUDED.managed_by,
            registry_path = EXCLUDED.registry_path,
            updated_at    = now()`,
		v.ID, v.Slug, v.Name, nullString(v.LegalName), nullString(v.HomepageURL),
		nullString(v.SupportURL), nullString(v.CountryCode), nullString(v.Notes),
		string(managedBy), nullString(v.RegistryPath),
		nullTime(v.CreatedAt), nullTime(v.UpdatedAt),
	)
	return wrap("vendor.Upsert", err)
}
