package domain_test

import (
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

func observation(t *testing.T, sourceID, version string, quality domain.QualityClass) domain.SourceObservation {
	t.Helper()
	return domain.SourceObservation{
		SourceID:          sourceID,
		ProductID:         "prd_routeros",
		Channel:           "stable",
		RawVersion:        version,
		NormalizedVersion: version,
		ObservedAt:        time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC),
		FirstObservedAt:   time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC),
		QualityClass:      quality,
		Official:          quality == domain.QualityOfficialManufacturer,
		Eligible:          true,
	}
}

// Two official sources disagreeing is the finding, not a problem the ladder is allowed
// to arbitrate. This test fails the moment somebody adds a same-tier tie-break, which is
// exactly what ADR-0020 says must not happen without a new decision record.
func TestAssessSourceConflictNeverResolvesTwoOfficialSources(t *testing.T) {
	t.Parallel()
	subject := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	other := observation(t, "src_b", "7.24.1", domain.QualityOfficialManufacturer)
	// A tie-break by recency would pick the subject; a tie-break by confidence would
	// need a number nobody has evidence for. Neither is allowed to fire.
	other.ObservedAt = subject.ObservedAt.Add(-72 * time.Hour)

	v := domain.AssessSourceConflict(subject, []domain.SourceObservation{other})

	if v.Status != domain.ConflictUnresolved {
		t.Fatalf("status = %q, want unresolved (%s)", v.Status, v.Reason)
	}
	if len(v.ConflictingVersions) != 1 || v.ConflictingVersions[0] != "7.24.1" {
		t.Errorf("conflicting versions = %v, want [7.24.1]", v.ConflictingVersions)
	}
	if len(v.OutrankedVersions) != 0 {
		t.Errorf("an official source was recorded as outranked: %v", v.OutrankedVersions)
	}
	if v.AuthorityRank != domain.QualityOfficialManufacturer.Authority() {
		t.Errorf("authority rank = %d, want %d", v.AuthorityRank, domain.QualityOfficialManufacturer.Authority())
	}
}

func TestAssessSourceConflictOutranksLowerAuthority(t *testing.T) {
	t.Parallel()
	subject := observation(t, "src_official", "7.24.3", domain.QualityOfficialManufacturer)
	other := observation(t, "src_forum", "7.24.1", domain.QualityTrustedCommunity)

	v := domain.AssessSourceConflict(subject, []domain.SourceObservation{other})

	if v.Status != domain.ConflictOutranked {
		t.Fatalf("status = %q, want outranked (%s)", v.Status, v.Reason)
	}
	if len(v.ConflictingVersions) != 0 {
		t.Errorf("a lower-authority disagreement blocked publication: %v", v.ConflictingVersions)
	}
	if len(v.OutrankedVersions) != 1 || v.OutrankedVersions[0] != "7.24.1" {
		t.Errorf("outranked versions = %v, want [7.24.1]", v.OutrankedVersions)
	}
	if len(v.OutrankedSourceIDs) != 1 || v.OutrankedSourceIDs[0] != "src_forum" {
		t.Errorf("outranked source ids = %v, want [src_forum]", v.OutrankedSourceIDs)
	}
}

// A lower-authority source never leapfrogs a higher one, whichever way round the
// comparison is made: the community source reporting a version the manufacturer does
// not is a conflict it cannot win on its own.
func TestAssessSourceConflictNeverLetsALowerTierOutrankAHigherOne(t *testing.T) {
	t.Parallel()
	subject := observation(t, "src_forum", "7.24.1", domain.QualityTrustedCommunity)
	other := observation(t, "src_official", "7.24.3", domain.QualityOfficialManufacturer)

	v := domain.AssessSourceConflict(subject, []domain.SourceObservation{other})

	if v.Status != domain.ConflictUnresolved {
		t.Fatalf("status = %q, want unresolved (%s)", v.Status, v.Reason)
	}
	if v.AuthorityRank != domain.QualityOfficialManufacturer.Authority() {
		t.Errorf("authority rank = %d, want the official tier", v.AuthorityRank)
	}
}

func TestAssessSourceConflictIgnoresIneligibleSources(t *testing.T) {
	t.Parallel()
	subject := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	other := observation(t, "src_b", "7.24.1", domain.QualityOfficialManufacturer)
	// A source FirmScout may not collect from, or whose health says its content cannot
	// be trusted, is not evidence of anything.
	other.Eligible = false

	v := domain.AssessSourceConflict(subject, []domain.SourceObservation{other})

	if v.Status != domain.ConflictNone {
		t.Fatalf("status = %q, want none (%s)", v.Status, v.Reason)
	}
}

func TestAssessSourceConflictIgnoresItsOwnPreviousObservation(t *testing.T) {
	t.Parallel()
	subject := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	previous := observation(t, "src_a", "7.24.2", domain.QualityOfficialManufacturer)

	// The caller passes everything it loaded, including this source's own stored row.
	// A source disagreeing with its own earlier claim is a new release, not a conflict.
	v := domain.AssessSourceConflict(subject, []domain.SourceObservation{previous, subject})

	if v.Status != domain.ConflictNone {
		t.Fatalf("status = %q, want none (%s)", v.Status, v.Reason)
	}
}

func TestAssessSourceConflictDeduplicatesAndSorts(t *testing.T) {
	t.Parallel()
	subject := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	others := []domain.SourceObservation{
		observation(t, "src_z", "7.25.0", domain.QualityOfficialManufacturer),
		observation(t, "src_b", "7.24.1", domain.QualityOfficialManufacturer),
		observation(t, "src_c", "7.24.1", domain.QualityOfficialManufacturer),
	}

	v := domain.AssessSourceConflict(subject, others)

	want := []string{"7.24.1", "7.25.0"}
	if len(v.ConflictingVersions) != len(want) {
		t.Fatalf("conflicting versions = %v, want %v", v.ConflictingVersions, want)
	}
	for i, s := range want {
		if v.ConflictingVersions[i] != s {
			t.Fatalf("conflicting versions = %v, want %v", v.ConflictingVersions, want)
		}
	}
	wantIDs := []string{"src_b", "src_c", "src_z"}
	for i, s := range wantIDs {
		if v.ConflictingSourceIDs[i] != s {
			t.Fatalf("conflicting source ids = %v, want %v", v.ConflictingSourceIDs, wantIDs)
		}
	}
}

func TestLaterObservationPrefersKnownDate(t *testing.T) {
	t.Parallel()
	dated := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	d, err := domain.NewExactDate(2026, time.August, 1)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	dated.ReleaseDate = d
	// The undated observation was seen later, which must not be enough: an undated
	// claim is weaker evidence of recency than a dated one.
	undated := observation(t, "src_b", "7.24.1", domain.QualityOfficialManufacturer)
	undated.ReleaseDate = domain.UnknownDate
	undated.ObservedAt = dated.ObservedAt.Add(48 * time.Hour)

	if !domain.LaterObservation(dated, undated) {
		t.Error("an undated observation outranked a dated one")
	}
	if domain.LaterObservation(undated, dated) {
		t.Error("a dated observation was outranked by an undated one")
	}
}

func TestLaterObservationPrefersLaterInstantOnEqualDates(t *testing.T) {
	t.Parallel()
	d, err := domain.NewExactDate(2026, time.August, 1)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	older := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	older.ReleaseDate = d
	newer := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	newer.ReleaseDate = d
	newer.ObservedAt = older.ObservedAt.Add(time.Hour)

	if !domain.LaterObservation(newer, older) {
		t.Error("the later observation instant did not break the tie")
	}
	if domain.LaterObservation(older, newer) {
		t.Error("the earlier observation instant won the tie")
	}
}

// The ordering never consults the version string, for the reason ADR-0017 gives: a
// version string has no defined order, so "9.99" is not evidence of anything.
func TestLaterObservationNeverComparesVersionStrings(t *testing.T) {
	t.Parallel()
	oldDate, err := domain.NewExactDate(2026, time.January, 1)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	newDate, err := domain.NewExactDate(2026, time.September, 1)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	bigVersionOldDate := observation(t, "src_a", "9.99.99", domain.QualityOfficialManufacturer)
	bigVersionOldDate.ReleaseDate = oldDate
	smallVersionNewDate := observation(t, "src_b", "1.0.0", domain.QualityOfficialManufacturer)
	smallVersionNewDate.ReleaseDate = newDate

	if !domain.LaterObservation(smallVersionNewDate, bigVersionOldDate) {
		t.Error("later-observation ordering used the version string rather than the release date")
	}
}

func TestSourceObservationValidate(t *testing.T) {
	t.Parallel()
	valid := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid observation was rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.SourceObservation)
	}{
		{"no source", func(o *domain.SourceObservation) { o.SourceID = "" }},
		{"no product", func(o *domain.SourceObservation) { o.ProductID = "" }},
		{"blank normalised version", func(o *domain.SourceObservation) { o.NormalizedVersion = "   " }},
		{"no raw version", func(o *domain.SourceObservation) { o.RawVersion = "" }},
		{"unknown quality class", func(o *domain.SourceObservation) { o.QualityClass = "made_up" }},
		{"no observation instant", func(o *domain.SourceObservation) { o.ObservedAt = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := observation(t, "src_a", "7.24.3", domain.QualityOfficialManufacturer)
			tc.mutate(&o)
			if err := o.Validate(); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

func TestSourceConflictRequiresTwoParticipants(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	valid := domain.SourceConflict{
		ID:         "cfl_1",
		ProductID:  "prd_routeros",
		Channel:    "stable",
		State:      domain.ConflictOpen,
		Versions:   []string{"7.24.1", "7.24.3"},
		SourceIDs:  []string{"src_a", "src_b"},
		DetectedAt: now,
		LastSeenAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid conflict was rejected: %v", err)
	}

	oneVersion := valid
	oneVersion.Versions = []string{"7.24.3"}
	if err := oneVersion.Validate(); err == nil {
		t.Error("a conflict with one version was accepted; a disagreement with one participant is not one")
	}

	oneSource := valid
	oneSource.SourceIDs = []string{"src_a", "src_a"}
	if err := oneSource.Validate(); err == nil {
		t.Error("a conflict where both participants are the same source was accepted")
	}

	resolvedWithoutTime := valid
	resolvedWithoutTime.State = domain.ConflictResolved
	if err := resolvedWithoutTime.Validate(); err == nil {
		t.Error("a resolved conflict with no resolution timestamp was accepted")
	}
}

func TestSourceConflictResolveRecordsWhoAndWhy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	open := domain.SourceConflict{
		ID:         "cfl_1",
		ProductID:  "prd_routeros",
		State:      domain.ConflictOpen,
		Versions:   []string{"7.24.1", "7.24.3"},
		SourceIDs:  []string{"src_a", "src_b"},
		DetectedAt: now,
	}

	resolved, err := open.Resolve(now.Add(time.Hour), "alex", "manufacturer confirmed 7.24.3")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.State != domain.ConflictResolved || resolved.ResolvedBy != "alex" {
		t.Fatalf("resolution not recorded: %+v", resolved)
	}
	if open.State != domain.ConflictOpen {
		t.Error("Resolve mutated the receiver rather than returning a copy")
	}
	if _, err := resolved.Resolve(now, "alex", "again"); err == nil {
		t.Error("an already-resolved conflict was resolved a second time")
	}
	if _, err := open.Resolve(now, "", "no actor"); err == nil {
		t.Error("a resolution with no actor was accepted")
	}
	if _, err := open.Resolve(now, "alex", ""); err == nil {
		t.Error("a resolution with no stated reason was accepted")
	}
}

func TestMergeParticipantsIncludesTheReportingSource(t *testing.T) {
	t.Parallel()
	got := domain.MergeParticipants("7.24.3", []string{"7.24.1", "7.24.3", ""})
	want := []string{"7.24.1", "7.24.3"}
	if len(got) != len(want) {
		t.Fatalf("MergeParticipants = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MergeParticipants = %v, want %v", got, want)
		}
	}
}
