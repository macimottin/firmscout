package domain

import "time"

// EventName identifies a domain or business event. These are values produced by the
// domain and dispatched by the application, which is why they carry no transport
// concerns: the same event is delivered in-process locally and forwarded to EventBridge
// on AWS without the domain knowing the difference.
type EventName string

const (
	// Pipeline events.
	EventSourceChecked        EventName = "SourceChecked"
	EventSourceChanged        EventName = "SourceChanged"
	EventSourceUnchanged      EventName = "SourceUnchanged"
	EventSourceFailed         EventName = "SourceFailed"
	EventSourceBroken         EventName = "SourceBroken"
	EventSourceRecovered      EventName = "SourceRecovered"
	EventSourceRelocated      EventName = "SourceRelocated"
	EventCandidateCreated     EventName = "CandidateReleaseCreated"
	EventCandidateValidated   EventName = "CandidateValidated"
	EventCandidateRejected    EventName = "CandidateRejected"
	EventReleasePublished     EventName = "ReleasePublished"
	EventReleaseWithdrawn     EventName = "ReleaseWithdrawn"
	EventHumanReviewRequested EventName = "HumanReviewRequested"
	EventCollectorFailed      EventName = "CollectorFailed"
	EventCollectorRecovered   EventName = "CollectorRecovered"

	// Multi-source conflict and human review events. A conflict is a fact about the
	// catalogue rather than about one candidate, which is why the detected and
	// resolved events carry a product subject and the review resolution carries the
	// item a human acted on.
	EventSourceConflictDetected EventName = "SourceConflictDetected"
	EventSourceConflictResolved EventName = "SourceConflictResolved"
	EventReviewItemResolved     EventName = "ReviewItemResolved"

	// Product and API events.
	EventSearchExecuted         EventName = "SearchExecuted"
	EventSearchReturnedNoResult EventName = "SearchReturnedNoResults"
	EventProductViewed          EventName = "ProductViewed"
	EventVendorViewed           EventName = "VendorViewed"
	EventAPIRequestCompleted    EventName = "APIRequestCompleted"
	EventAPIQuotaExceeded       EventName = "APIQuotaExceeded"
	EventAPIKeyCreated          EventName = "APIKeyCreated"
	EventCorrectionSubmitted    EventName = "CorrectionSubmitted"

	// AI events.
	EventAIRepairRequested EventName = "AIRepairRequested"
	EventAIRepairCompleted EventName = "AIRepairCompleted"
)

// Event is a fact about something that happened, carrying only the attributes a
// consumer needs.
//
// Attributes are deliberately a map of strings rather than a typed payload per event.
// The privacy filter in the analytics adapter has to inspect every attribute before
// storage, and a uniform shape makes that filter one piece of code rather than one per
// event type. The cost is losing compile-time attribute checking, which is the right
// trade for data that must be redactable.
type Event struct {
	Name       EventName
	OccurredAt time.Time
	// SubjectType and SubjectID identify what the event is about, for example
	// ("source", "src_01H...") or ("release", "rel_01H...").
	SubjectType string
	SubjectID   string
	VendorSlug  string
	ProductSlug string
	Attributes  map[string]string
}

// NewEvent constructs an event with the given attributes.
func NewEvent(name EventName, at time.Time, subjectType, subjectID string) Event {
	return Event{
		Name:        name,
		OccurredAt:  at,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		Attributes:  map[string]string{},
	}
}

// With returns a copy of the event with an additional attribute set. Attribute values
// must never contain secrets, credentials, API keys or customer inventory contents;
// the analytics adapter enforces a redaction list, but the first line of defence is
// not putting them here.
func (e Event) With(key, value string) Event {
	attrs := make(map[string]string, len(e.Attributes)+1)
	for k, v := range e.Attributes {
		attrs[k] = v
	}
	attrs[key] = value
	e.Attributes = attrs
	return e
}

// WithProduct tags the event with the vendor and product it concerns.
func (e Event) WithProduct(vendorSlug, productSlug string) Event {
	e.VendorSlug = vendorSlug
	e.ProductSlug = productSlug
	return e
}
