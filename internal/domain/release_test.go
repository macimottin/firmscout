package domain

import "testing"

// TestPublicSourceKindCoversEveryDeclaredSourceType is the revert detector for adding
// a thirteenth SourceType without deciding what it renders as: every value
// ValidSourceType accepts must map to one of the six public kinds a release's source
// and a product's officialSources are allowed to claim.
func TestPublicSourceKindCoversEveryDeclaredSourceType(t *testing.T) {
	publicKinds := map[string]bool{
		PublicKindHTML: true, PublicKindText: true, PublicKindJSON: true,
		PublicKindXML: true, PublicKindPDF: true, PublicKindRSSAtom: true,
	}
	for _, st := range []SourceType{
		SourceTypeRESTAPI, SourceTypeGraphQLAPI, SourceTypeRSSAtom, SourceTypeXMLFeed,
		SourceTypeJSONEndpoint, SourceTypeHTMLPage, SourceTypePDFReleaseNotes,
		SourceTypeDownloadPortal, SourceTypeGitHubReleases, SourceTypeSitemap,
		SourceTypeAuthenticatedPortal, SourceTypeManual,
	} {
		if !ValidSourceType(st) {
			t.Fatalf("test fixture %q is not a valid source type; the fixture list has drifted from ValidSourceType", st)
		}
		got := PublicSourceKind(st)
		if !publicKinds[got] {
			t.Errorf("PublicSourceKind(%q) = %q, which is not one of the declared public kinds", st, got)
		}
	}
}

// TestPublicSourceKindSpecificMappings pins the mappings a reader is most likely to
// depend on directly, so a change to any of them shows up as a named failure rather
// than only as a coverage assertion above.
func TestPublicSourceKindSpecificMappings(t *testing.T) {
	cases := []struct {
		in   SourceType
		want string
	}{
		{SourceTypeHTMLPage, PublicKindHTML},
		{SourceTypeRSSAtom, PublicKindRSSAtom},
		{SourceTypeXMLFeed, PublicKindXML},
		{SourceTypeSitemap, PublicKindXML},
		{SourceTypePDFReleaseNotes, PublicKindPDF},
		{SourceTypeRESTAPI, PublicKindJSON},
		{SourceTypeGitHubReleases, PublicKindJSON},
		{SourceTypeManual, PublicKindText},
		// An empty or unrecognised source type -- the fallback a NULL evidence.source_type
		// with no resolvable source row hits -- must default to the vaguest kind rather
		// than a guessed structure.
		{SourceType(""), PublicKindText},
		{SourceType("something_new_nobody_taught_this_function"), PublicKindText},
	}
	for _, c := range cases {
		if got := PublicSourceKind(c.in); got != c.want {
			t.Errorf("PublicSourceKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReleaseSourceAndEvidenceAreZeroUntilThereadPath documents the contract Source
// and Evidence's doc comments make: a Release assembled by hand -- the shape every
// write-path caller (Insert, Validate) actually sees -- carries no source or evidence
// detail, because that is the read path's job, not the write path's. See
// internal/adapters/postgres's TestReleaseSourceAndEvidencePopulatedOnEveryReadPath
// for the other half of this contract, against a real database.
func TestReleaseSourceAndEvidenceAreZeroUntilTheReadPath(t *testing.T) {
	var r Release
	if r.Source != (ReleaseSource{}) {
		t.Errorf("a zero-value Release has a non-zero Source: %+v", r.Source)
	}
	if r.Evidence != (ReleaseEvidence{}) {
		t.Errorf("a zero-value Release has a non-zero Evidence: %+v", r.Evidence)
	}
}
