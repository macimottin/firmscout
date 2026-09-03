package postgres

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/macimottin/firmscout/internal/domain"
)

// These tests need no database. They cover the pure parts of the adapter -- error
// translation, cursors, date and version marshalling, identifier generation -- which is
// where a mistake would be silently wrong rather than loudly broken.

func TestTranslateMapsDriverErrorsToDomainErrors(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
		// leaks names a driver error that must NOT be reachable through the result.
		leaks error
	}{
		{
			name:  "no rows becomes not found and drops the driver error",
			in:    pgx.ErrNoRows,
			want:  domain.ErrNotFound,
			leaks: pgx.ErrNoRows,
		},
		{
			name: "wrapped no rows still becomes not found",
			in:   errors.Join(errors.New("context"), pgx.ErrNoRows),
			want: domain.ErrNotFound,
			// The driver error is dropped along with everything wrapped with it,
			// which is the price of guaranteeing it cannot escape.
			leaks: pgx.ErrNoRows,
		},
		{
			name: "unique violation becomes conflict",
			in:   &pgconn.PgError{Code: sqlstateUniqueViolation, Message: "duplicate key"},
			want: domain.ErrConflict,
		},
		{
			name: "exclusion violation becomes conflict",
			in:   &pgconn.PgError{Code: sqlstateExclusionViolation},
			want: domain.ErrConflict,
		},
		{
			name: "check violation becomes validation",
			in:   &pgconn.PgError{Code: sqlstateCheckViolation, Message: "releases_date_precision"},
			want: domain.ErrValidation,
		},
		{
			name: "foreign key violation becomes validation",
			in:   &pgconn.PgError{Code: sqlstateForeignKeyViolation},
			want: domain.ErrValidation,
		},
		{
			name: "not null violation becomes validation",
			in:   &pgconn.PgError{Code: sqlstateNotNullViolation},
			want: domain.ErrValidation,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translate(tc.in)
			if !errors.Is(got, tc.want) {
				t.Fatalf("translate(%v) = %v, want it to match %v", tc.in, got, tc.want)
			}
			if tc.leaks != nil && errors.Is(got, tc.leaks) {
				t.Errorf("translate leaked %v through %v", tc.leaks, got)
			}
			// The PostgreSQL detail stays reachable for diagnostics.
			var pgErr *pgconn.PgError
			if errors.As(tc.in, &pgErr) {
				var out *pgconn.PgError
				if !errors.As(got, &out) {
					t.Error("the PgError is no longer reachable for diagnostics")
				}
			}
		})
	}
}

func TestTranslateLeavesRetryableAndUnknownErrorsAlone(t *testing.T) {
	retryable := &pgconn.PgError{Code: sqlstateSerializationFail}
	if got := translate(retryable); got != error(retryable) {
		t.Errorf("a serialization failure was translated to %v; the caller must be able to retry it", got)
	}
	if errors.Is(translate(retryable), domain.ErrConflict) {
		t.Error("a serialization failure was reported as a conflict, hiding that a retry is correct")
	}

	other := errors.New("connection reset")
	if got := translate(other); got != other {
		t.Errorf("translate rewrote an unrelated error to %v", got)
	}
	if translate(nil) != nil {
		t.Error("translate(nil) is not nil")
	}
}

func TestWrapKeepsTheSentinelReachable(t *testing.T) {
	err := wrap("vendor.GetByID", pgx.ErrNoRows)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("wrap lost the sentinel: %v", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		t.Error("wrap leaked pgx.ErrNoRows")
	}
	if wrap("op", nil) != nil {
		t.Error("wrap(op, nil) is not nil")
	}
	if !errors.Is(notFound("op"), domain.ErrNotFound) {
		t.Error("notFound does not match domain.ErrNotFound")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	token := encodeCursor("2026-02-14T00:00:00Z", "rel_1")
	parts, err := decodeCursor(token, 2)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if len(parts) != 2 || parts[0] != "2026-02-14T00:00:00Z" || parts[1] != "rel_1" {
		t.Errorf("round trip = %v", parts)
	}

	if parts, err := decodeCursor("", 1); err != nil || parts != nil {
		t.Errorf("empty cursor = (%v, %v), want (nil, nil)", parts, err)
	}
	if _, err := decodeCursor("not base64!!", 1); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("malformed cursor = %v, want domain.ErrValidation", err)
	}
	// A cursor of the wrong shape is an error rather than a silent restart from the
	// first page, which is how a client ends up looping forever.
	if _, err := decodeCursor(encodeCursor("a"), 2); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("wrong-arity cursor = %v, want domain.ErrValidation", err)
	}
	// The separator must survive values that contain URL-unsafe characters.
	token = encodeCursor("a/b+c=d", "e f")
	parts, err = decodeCursor(token, 2)
	if err != nil || parts[0] != "a/b+c=d" || parts[1] != "e f" {
		t.Errorf("round trip of awkward values = (%v, %v)", parts, err)
	}
}

func TestDateParamsRoundTrip(t *testing.T) {
	exact, err := domain.NewExactDate(2026, time.February, 14)
	if err != nil {
		t.Fatal(err)
	}
	month, err := domain.NewMonthDate(2026, time.February)
	if err != nil {
		t.Fatal(err)
	}
	year, err := domain.NewYearDate(2026)
	if err != nil {
		t.Fatal(err)
	}

	for _, in := range []domain.PartialDate{exact, month, year, domain.UnknownDate} {
		anchor, precision := dateParams(in)
		out, err := partialDate(anchor, precision)
		if err != nil {
			t.Fatalf("partialDate(%v): %v", in, err)
		}
		if out.Precision() != in.Precision() || out.String() != in.String() {
			t.Errorf("round trip of %q gave %q", in.String(), out.String())
		}
	}

	// An unknown date must store NULL, because the schema's CHECK requires it.
	if anchor, precision := dateParams(domain.UnknownDate); anchor != nil || precision != string(domain.PrecisionUnknown) {
		t.Errorf("unknown date stored as (%v, %q), want (nil, unknown)", anchor, precision)
	}

	// A stored row that violates the anchoring rule is rejected on read rather than
	// becoming a date with invented precision.
	bad := time.Date(2026, time.February, 14, 0, 0, 0, 0, time.UTC)
	if _, err := partialDate(&bad, string(domain.PrecisionMonthOnly)); err == nil {
		t.Error("partialDate accepted a month-precision date anchored on day 14")
	}
}

func TestPublicationDateDropsPrecisionItCannotStore(t *testing.T) {
	// The schema has no precision column for publication_date. Rather than store an
	// anchor that would read back as a real day, a coarser value is not stored at all.
	month, err := domain.NewMonthDate(2026, time.February)
	if err != nil {
		t.Fatal(err)
	}
	if got := publicationDateParam(month); got != nil {
		t.Errorf("a month-precision publication date was stored as %v; it would read back as a day", got)
	}

	exact, err := domain.NewExactDate(2026, time.February, 14)
	if err != nil {
		t.Fatal(err)
	}
	anchor := publicationDateParam(exact)
	if anchor == nil {
		t.Fatal("an exact publication date was not stored")
	}
	back, err := publicationDate(anchor)
	if err != nil {
		t.Fatalf("publicationDate: %v", err)
	}
	if back.String() != "2026-02-14" {
		t.Errorf("round trip = %q, want 2026-02-14", back.String())
	}
	if got, err := publicationDate(nil); err != nil || got.Known() {
		t.Errorf("NULL publication date = (%v, %v), want the unknown date", got, err)
	}
}

func TestVersionRoundTripPreservesTheRawForm(t *testing.T) {
	// Vendor versions are not semantic versions. The raw form is what the vendor
	// published and must survive persistence character for character.
	for _, raw := range []string{"3.003.0015.001", "CollabOS 2.1.B (2.1.121)", "7.24.2"} {
		v, err := domain.NewVersionString(raw)
		if err != nil {
			t.Fatalf("NewVersionString(%q): %v", raw, err)
		}
		out, err := version(v.Raw(), v.Normalized())
		if err != nil {
			t.Fatalf("version(%q): %v", raw, err)
		}
		if out.Raw() != raw || out.Normalized() != v.Normalized() {
			t.Errorf("round trip of %q gave raw %q normalized %q", raw, out.Raw(), out.Normalized())
		}
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct{ in, def, max, want int }{
		{0, 50, 500, 50},
		{-1, 50, 500, 50},
		{10, 50, 500, 10},
		{5000, 50, 500, 500},
		{500, 50, 500, 500},
	}
	for _, c := range cases {
		if got := clampLimit(c.in, c.def, c.max); got != c.want {
			t.Errorf("clampLimit(%d, %d, %d) = %d, want %d", c.in, c.def, c.max, got, c.want)
		}
	}
}

func TestULIDGeneratorIsPrefixedUniqueAndSortable(t *testing.T) {
	g := ulidGenerator{}

	const n = 2000
	ids := make([]string, 0, n)
	seen := map[string]bool{}
	for range n {
		id := g.NewID("src")
		if len(id) != len("src_")+26 {
			t.Fatalf("id %q has length %d, want %d", id, len(id), len("src_")+26)
		}
		if id[:4] != "src_" {
			t.Fatalf("id %q is not prefixed", id)
		}
		for _, c := range id[4:] {
			if !containsRune(crockford, c) {
				t.Fatalf("id %q contains %q, which is not Crockford base32", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}

	// Identifiers minted in order sort in order, which is what keyset pagination on
	// id relies on. Ties within the same millisecond are broken by randomness, so
	// compare only the timestamp prefix.
	prefixes := make([]string, len(ids))
	for i, id := range ids {
		prefixes[i] = id[4 : 4+10]
	}
	if !sort.StringsAreSorted(prefixes) {
		t.Error("identifiers minted in order do not sort in order")
	}

	if got := g.NewID(""); len(got) != 26 {
		t.Errorf("unprefixed id %q has length %d, want 26", got, len(got))
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestNullHelpers(t *testing.T) {
	if nullString("") != nil {
		t.Error("nullString(\"\") is not nil")
	}
	if got := nullString("x"); got == nil || *got != "x" {
		t.Error("nullString lost the value")
	}
	if nullTime(time.Time{}) != nil {
		t.Error("nullTime(zero) is not nil")
	}
	if nullInt(0) != nil {
		t.Error("nullInt(0) is not nil")
	}
	if got := nullInt(900); got == nil || *got != 900 {
		t.Error("nullInt lost the value")
	}
	if str(nil) != "" || tim(nil) != (time.Time{}) || integer(nil) != 0 ||
		float(nil) != 0 || int64val(nil) != 0 {
		t.Error("a null reader did not produce a zero value")
	}
}
