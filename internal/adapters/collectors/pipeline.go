package collectors

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/internal/domain"
)

// This file holds what the two engines share: the value pipeline that turns a located
// raw string into a field value, and the candidate builder that assembles those values
// into a domain.CandidateRelease.
//
// Both engines locate values differently -- one by CSS selector inside a DOM node, the
// other by capture group inside a regex match -- and identically thereafter. Keeping
// the "thereafter" in one place is what makes the two engines agree about precision,
// confidence, dedupe keys and unmapped labels, rather than each engine having its own
// nearly-identical opinion.

// container is one repeating unit an engine found: an HTML element for
// html_selectors, a regex match for text_regex.
type container interface {
	// value returns the raw text a field's selector locates, and whether the
	// selector located anything at all. A located-but-empty value is ("", true):
	// that distinction is the difference between "the page changed shape" and "the
	// vendor left this blank".
	// A field declaring a literal never reaches an engine: buildCandidate answers it
	// directly. An error means the document said something the config did not
	// anticipate -- currently only a selector matching several elements under the
	// default MultipleError policy -- and is handled exactly like an unmapped label.
	value(f *FieldSpec) (string, bool, error)
	// excerpt returns the evidence text for this container.
	excerpt() string
}

// fieldRaw obtains a field's raw value: the declared literal when the field states a
// constant, otherwise whatever the engine's container locates.
//
// The literal short-circuits before any engine is consulted, which is what makes
// FieldSpec.Value engine-independent and what lets validation forbid pairing it with a
// selector: there is no path here that could read both and prefer one.
func fieldRaw(c container, f *FieldSpec) (string, bool, error) {
	if f.literal {
		return f.Value, true, nil
	}
	return c.value(f)
}

// buildCandidate assembles one candidate from one container.
//
// It returns ok=false when the container must be skipped, always with at least one
// warning explaining why. Skipping is never silent: a page whose shape changed
// produces a run full of warnings, which is a repair signal, whereas a page that
// silently produces nothing looks exactly like a page with no new releases.
func buildCandidate(cfg *Config, index int, c container) (domain.CandidateRelease, []string, bool) {
	var warnings []string
	spec := &cfg.Spec.Extract
	label := fmt.Sprintf("%s: container %d", cfg.Metadata.ID, index)

	missingOptional := 0
	// resolve runs the whole value pipeline for one declared field.
	// found reports whether the field is declared at all; ok reports whether the
	// candidate survives.
	resolve := func(name string) (val string, declared bool, ok bool) {
		f, declared := spec.Fields[name]
		if !declared {
			return "", false, true
		}
		raw, located, verr := fieldRaw(c, f)
		out, err := applyPipeline(f, raw)
		if verr != nil {
			err = verr
		}
		if err != nil {
			// An unmapped label is a hard error for this candidate, never a
			// pass-through: a vendor badge nobody has interpreted must not be
			// guessed at.
			warnings = append(warnings, fmt.Sprintf("%s: field %q: %v; candidate skipped", label, name, err))
			return "", true, false
		}
		if !located || strings.TrimSpace(out) == "" {
			if isDateField(name) {
				// Handled by the date path, which produces an unknown date rather
				// than skipping the candidate.
				return "", true, true
			}
			if name == FieldVersion {
				warnings = append(warnings, fmt.Sprintf("%s: field %q produced no value; candidate skipped "+
					"(a candidate with no version cannot be stored or deduplicated)", label, name))
				return "", true, false
			}
			if f.IsRequired() {
				warnings = append(warnings, fmt.Sprintf("%s: required field %q produced no value; candidate skipped", label, name))
				return "", true, false
			}
			missingOptional++
			warnings = append(warnings, fmt.Sprintf("%s: optional field %q produced no value; confidence lowered", label, name))
			return "", true, true
		}
		return out, true, true
	}

	rawVersion, _, ok := resolve(FieldVersion)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}

	var version domain.VersionString
	var err error
	normalized, declaredNorm, ok := resolve(FieldNormalizedVersion)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}
	if declaredNorm && normalized != "" {
		version, err = domain.NewVersionStringWithNormalized(rawVersion, normalized)
	} else {
		version, err = domain.NewVersionString(rawVersion)
	}
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("%s: version %q rejected: %v; candidate skipped", label, rawVersion, err))
		return domain.CandidateRelease{}, warnings, false
	}

	channel, declaredChannel, ok := resolve(FieldChannel)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}
	if !declaredChannel {
		channel = spec.Channel
	}

	hint, declaredHint, ok := resolve(FieldProductHint)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}
	if !declaredHint || hint == "" {
		hint = cfg.Spec.ProductMatch.Product
		if hint == "" {
			hint = cfg.Spec.ProductMatch.Family
		}
	}

	hardware, _, ok := resolve(FieldHardwareRevision)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}
	region, _, ok := resolve(FieldRegion)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}
	deployment, _, ok := resolve(FieldDeploymentMode)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}
	notesURL, _, ok := resolve(FieldReleaseNotesURL)
	if !ok {
		return domain.CandidateRelease{}, warnings, false
	}

	releaseDate, dateWarns := resolveDate(cfg, label, c, FieldReleaseDate, &missingOptional)
	warnings = append(warnings, dateWarns...)
	publicationDate, pubWarns := resolveDate(cfg, label, c, FieldPublicationDate, &missingOptional)
	warnings = append(warnings, pubWarns...)

	app := domain.Applicability{
		HardwareRevision: firstNonEmpty(hardware, spec.Applicability.HardwareRevision),
		Region:           firstNonEmpty(region, spec.Applicability.Region),
		Channel:          channel,
		DeploymentMode:   firstNonEmpty(deployment, spec.Applicability.DeploymentMode),
		Note:             spec.Applicability.Note,
	}

	confidence := spec.Confidence.Base - float64(missingOptional)*spec.Confidence.MissingOptionalPenalty
	if confidence < spec.Confidence.Floor {
		confidence = spec.Confidence.Floor
	}

	candidate := domain.CandidateRelease{
		ProductMatchHint: hint,
		// Resolution against the catalogue is a validation gate in the application
		// layer, which is where an ambiguous match can be routed to a human. A
		// collector states honestly that it has not resolved anything.
		ProductMatchStatus: domain.MatchUnresolved,
		Version:            version,
		ReleaseType:        domain.ReleaseType(spec.ReleaseType),
		Applicability:      app,
		ReleaseDate:        releaseDate,
		PublicationDate:    publicationDate,
		ReleaseNotesURL:    notesURL,
		Confidence:         confidence,
		DedupeKey:          domain.ComputeDedupeKey(hint, version, app),
	}
	// State is deliberately left unset: lifecycle belongs to the application layer,
	// which stamps "discovered" and transitions to "extracted" when it persists.

	return sdk.WithExcerpt(candidate, c.excerpt()), warnings, true
}

// resolveDate produces a PartialDate at exactly the declared precision.
//
// Every failure path here lands on domain.UnknownDate. There is no path that invents a
// day, a month or a year: a date FirmScout could not read is rendered as absent, never
// as an approximation the reader would have no way to distinguish from a fact.
func resolveDate(cfg *Config, label string, c container, name string, missingOptional *int) (domain.PartialDate, []string) {
	f, ok := cfg.Spec.Extract.Fields[name]
	if !ok {
		return domain.UnknownDate, nil
	}
	precision := domain.DatePrecision(f.Precision)
	if precision == domain.PrecisionUnknown {
		return domain.UnknownDate, nil
	}

	raw, located, verr := fieldRaw(c, f)
	text, err := applyPipeline(f, raw)
	if verr != nil {
		err = verr
	}
	if err != nil {
		*missingOptional++
		return domain.UnknownDate, []string{fmt.Sprintf("%s: field %q: %v; release date recorded as unknown", label, name, err)}
	}
	text = strings.TrimSpace(text)
	if !located || text == "" {
		*missingOptional++
		return domain.UnknownDate, []string{fmt.Sprintf("%s: field %q produced no value; release date recorded as unknown", label, name)}
	}

	d, err := parseDate(text, f.DateFormat, precision)
	if err != nil {
		*missingOptional++
		return domain.UnknownDate, []string{fmt.Sprintf("%s: field %q: %v; release date recorded as unknown rather than guessed", label, name, err)}
	}
	return d, nil
}

// parseDate converts text into a PartialDate at the declared precision.
//
// The precision is declared by the config and enforced here: a month_only field
// constructs through domain.NewMonthDate, which has no parameter for a day, so there
// is no code path -- not a bug, not a future edit -- by which this function could
// return a day the source did not publish.
//
// ZONES, AND WHY THERE IS NO UTC NORMALISATION HERE. The calendar components below are
// read in the offset the source published them in. The rejected alternative is the
// obvious one, and it was in this function: convert the parsed instant to UTC first,
// then take Year/Month/Day. That answers with OUR calendar instead of the vendor's. A
// source writing "Wed, 12 Aug 2026 17:00:00 -0700" published the twelfth; normalised to
// UTC it becomes the thirteenth, and FirmScout then stores 13 August at exact_day
// precision, where nothing downstream can tell it from a fact -- gate 5 only bounds
// plausibility, the database CHECK only enforces the anchoring rule, and the presenter
// faithfully renders whatever it is handed. Manufacturing a day the source never
// published is the exact failure ADR-0017 exists to prevent.
//
// The same rule settles what a zone-bearing layout means for a reduced-precision field,
// which config validation accepts because layoutHasDay is false for a layout such as
// "Jan 2006 -0700": one rule for all three precisions, namely take the wall clock the
// source wrote and discard everything below the declared precision. "Aug 2026 +0200" is
// therefore August 2026, not the July a UTC normalisation would produce by rolling
// midnight on the first backwards. validateDateField is written against this rule and
// says so.
//
// It also covers the zoneless case with no special path: time.Parse with a layout that
// carries no zone yields a UTC time, and reading UTC components off a UTC time is the
// identity. That equivalence is precisely why the old .UTC() call looked harmless --
// it conflated "already UTC because the source stated no offset" with "converted to UTC
// from a stated offset". Do not reintroduce it.
func parseDate(text, layout string, precision domain.DatePrecision) (domain.PartialDate, error) {
	var t time.Time

	switch {
	case layout == EpochSecondsFormat:
		secs, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return domain.UnknownDate, fmt.Errorf("%q is not a Unix timestamp in seconds", text)
		}
		// The one deliberate exception to the rule above, because an epoch names an
		// instant and states no offset at all: there is no source calendar to
		// preserve. UTC is chosen over the machine's local zone so that the recorded
		// day does not depend on where the collector happened to run.
		t = time.Unix(secs, 0).UTC()
	case layout == "" && precision == domain.PrecisionYearOnly:
		year, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return domain.UnknownDate, fmt.Errorf("%q is not a four-digit year", text)
		}
		return domain.NewYearDate(year)
	case layout == "":
		return domain.UnknownDate, fmt.Errorf("no date_format declared for precision %s", precision)
	default:
		parsed, err := time.Parse(layout, strings.TrimSpace(text))
		if err != nil {
			return domain.UnknownDate, fmt.Errorf("%q does not match layout %q", text, layout)
		}
		t = parsed
	}

	// t carries the source's own offset (or UTC, when the source stated none), and
	// Year/Month/Day report in t's location, so these are the components the vendor
	// wrote. Nothing between here and the constructor may move t to another zone.
	switch precision {
	case domain.PrecisionExactDay:
		return domain.NewExactDate(t.Year(), t.Month(), t.Day())
	case domain.PrecisionMonthOnly:
		return domain.NewMonthDate(t.Year(), t.Month())
	case domain.PrecisionYearOnly:
		return domain.NewYearDate(t.Year())
	default:
		return domain.UnknownDate, nil
	}
}

// applyPipeline runs a field's regex, transform pipeline and value map over a raw
// string.
//
// Order is fixed and documented: regex first (it selects the substring the transforms
// operate on), then the transform pipeline in list order, then the field's map unless
// the pipeline already applied it explicitly.
func applyPipeline(f *FieldSpec, raw string) (string, error) {
	v := raw
	if f.re != nil {
		v = applyRegex(f.re, v)
	}

	mapped := false
	for i := range f.Transform {
		t := &f.Transform[i]
		switch t.Name {
		case TransformTrim:
			v = strings.TrimSpace(v)
		case TransformLowercase:
			v = strings.ToLower(v)
		case TransformUppercase:
			v = strings.ToUpper(v)
		case TransformStripPrefix:
			v = strings.TrimPrefix(v, t.Arg)
		case TransformStripSuffix:
			v = strings.TrimSuffix(v, t.Arg)
		case TransformRegexExtract:
			v = applyRegex(t.re, v)
		case TransformRegexReplace:
			v = t.re.ReplaceAllString(v, t.With)
		case TransformMap:
			out, err := applyMap(f.Map, v)
			if err != nil {
				return "", err
			}
			v, mapped = out, true
		case TransformParseDate:
			// Parsing happens in resolveDate, which knows the declared precision.
			// Listing it in the pipeline documents intent and is a no-op here.
		}
	}

	if len(f.Map) > 0 && !mapped {
		out, err := applyMap(f.Map, v)
		if err != nil {
			return "", err
		}
		v = out
	}
	return v, nil
}

// applyRegex keeps capture group 1 when the pattern has one, otherwise the whole
// match, and the empty string when the pattern does not match at all.
func applyRegex(re *regexp.Regexp, v string) string {
	m := re.FindStringSubmatch(v)
	if m == nil {
		return ""
	}
	if len(m) > 1 {
		return m[1]
	}
	return m[0]
}

// applyMap is an exact-match lookup. A value with no entry is an error, deliberately:
// a vendor label nobody has interpreted is not evidence for any channel, and treating
// it as one is how an unreviewed "development" build reaches a stable feed.
func applyMap(m map[string]string, v string) (string, error) {
	if len(m) == 0 {
		return v, nil
	}
	if out, ok := m[v]; ok {
		return out, nil
	}
	if strings.TrimSpace(v) == "" {
		// An absent value is a missing field, not an unmapped label; let the caller's
		// missing-field handling deal with it.
		return "", nil
	}
	return "", fmt.Errorf("value %q has no entry in the field's map (%s)", v, strings.Join(sortedKeys(m), ", "))
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// collapseWhitespace renders element or match text as a single line, which is what an
// evidence excerpt should be: markup indentation is not evidence.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
