package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// The internal review surface.
//
// # Read this before adding a route to this file
//
// These four endpoints are unauthenticated and two of them write to the catalogue: an
// accept publishes a release that a validation gate refused. The only things standing
// between that and a defacement API are (a) the two switches in NewServer, which default
// off, and (b) network placement, which is an operator's job and not something any code
// here can verify. docs/architecture/api.md §11 says so out loud, and ADR-0021 records
// why this was shipped anyway rather than waiting for authentication that is out of
// scope.
//
// Two switches, and a third that is not in this repository's Go code at all: apps/web's
// FIRMSCOUT_REVIEW_UI_ENABLED. Counting only the two here is the failure mode the first
// real deployment would have had: an operator who placed this server exactly as
// prescribed could still make the surface internet-reachable by switching on a
// first-party client that sits somewhere able to reach it. Nothing reached users --
// FirmScout has never been deployed, no image has ever been built -- and the client's
// write path was removed inside the same phase. The count belongs in this comment anyway,
// because this is where a reader arrives asking what protects these routes.
//
// What follows from that, concretely:
//
//   - Nothing here is in docs/api/openapi.yaml. That document is the machine-readable
//     *public* contract, and a client generator must not emit bindings for this.
//   - Every response is Cache-Control: no-store with no ETag. These routes never pass
//     through CacheHeaders, so there is no shared cache anywhere in the path that could
//     hold a moderation queue.
//   - The reviewer's identity is a header the caller asserted. It is recorded with
//     actor_authenticated = false and it is not, at any point, treated as a credential.

// maxReviewDecisionBody bounds a decision request body. A reason is a sentence, and an
// unbounded reader on an unauthenticated POST is a memory-exhaustion invitation.
const maxReviewDecisionBody = 8 << 10

// maxReviewActorLength bounds the asserted reviewer name. The value is stored verbatim
// in audit_events.actor_id and shown in a reviewer UI; a caller who can put a kilobyte
// of anything into an audit row has turned the audit trail into a scratch pad.
const maxReviewActorLength = 200

// reviewItemIDPattern is the shape a review item id has. It is checked before any
// repository is touched, so a malformed id costs a regexp match rather than a query.
var reviewItemIDPattern = regexp.MustCompile(`^rev_[A-Za-z0-9]{20,32}$`)

// reviewActorDetail is the whole explanation a caller gets for a missing actor header.
// It says what the header is *for* as well as what it is not, because the natural
// reading of "identify yourself" is "authenticate", and that is precisely the thing
// this header does not do.
const reviewActorDetail = "Name the reviewer in the " + HeaderReviewActor +
	" header. It is an asserted identity, not a credential: FirmScout has no login yet " +
	"and records the name unverified."

// Review queue page bounds, mirroring application.DefaultReviewPageSize and
// MaxReviewPageSize. They are restated here because the HTTP layer clamps the caller's
// parameter and the use case clamps its own input, and the two agreeing is what makes
// the documented default true.
const (
	reviewLimitDefault = application.DefaultReviewPageSize
	reviewLimitMax     = application.MaxReviewPageSize
)

// handleListReviewItems serves a filtered page of the queue.
func (s *Server) handleListReviewItems(w http.ResponseWriter, r *http.Request) {
	s.noStore(w)
	query := r.URL.Query()

	limit, err := parseLimit(r, reviewLimitDefault, reviewLimitMax)
	if err != nil {
		WriteProblem(w, r, InvalidParameter(r, err.Error()))
		return
	}

	q := application.ReviewQueueQuery{
		// The vocabularies themselves are checked by the use case, which owns them.
		// Re-declaring the ten review kinds here would be a second copy of a CHECK
		// constraint, and the two would drift the first time one is added.
		States:      query["state"],
		Kinds:       query["kind"],
		SLAClasses:  query["sla"],
		VendorSlug:  strings.TrimSpace(query.Get("vendor")),
		ProductSlug: strings.TrimSpace(query.Get("product")),
		Limit:       limit,
		Cursor:      query.Get("cursor"),
	}
	if q.VendorSlug != "" && !domain.ValidSlug(q.VendorSlug) {
		WriteProblem(w, r, InvalidParameter(r, "The 'vendor' filter must be a lowercase kebab-case slug."))
		return
	}
	if q.ProductSlug != "" && !domain.ValidSlug(q.ProductSlug) {
		WriteProblem(w, r, InvalidParameter(r, "The 'product' filter must be a lowercase kebab-case slug."))
		return
	}

	page, err := s.reviewQueue.Execute(r.Context(), q)
	if err != nil {
		s.writeReviewError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentReviewQueue(page))
}

// handleGetReviewItem serves one item in full.
func (s *Server) handleGetReviewItem(w http.ResponseWriter, r *http.Request) {
	s.noStore(w)
	id, ok := s.reviewItemID(w, r)
	if !ok {
		return
	}
	detail, err := s.reviewItems.Execute(r.Context(), id)
	if err != nil {
		s.writeReviewError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentReviewItemDetail(detail))
}

// handleAcceptReviewItem publishes the item's candidate over the gate that stopped it.
func (s *Server) handleAcceptReviewItem(w http.ResponseWriter, r *http.Request) {
	s.decideReviewItem(w, r, s.review.Accept)
}

// handleRejectReviewItem rejects the item's candidate.
func (s *Server) handleRejectReviewItem(w http.ResponseWriter, r *http.Request) {
	s.decideReviewItem(w, r, s.review.Reject)
}

// reviewDecision is DecideReviewItem.Accept or .Reject. The two handlers differ only in
// which one they pass, so the id check, the actor rule, the body limit and the error
// mapping are written once: an accept that validated its body differently from a reject
// would be a gap somebody eventually walks through.
type reviewDecision func(context.Context, application.ReviewDecisionInput) (application.ReviewDecisionResult, error)

func (s *Server) decideReviewItem(w http.ResponseWriter, r *http.Request, decide reviewDecision) {
	s.noStore(w)

	id, ok := s.reviewItemID(w, r)
	if !ok {
		return
	}
	actor, ok := s.reviewActor(w, r)
	if !ok {
		return
	}
	reason, ok := s.reviewReason(w, r)
	if !ok {
		return
	}

	result, err := decide(r.Context(), application.ReviewDecisionInput{
		ItemID: id,
		Actor:  actor,
		// False, always, in this phase. It is a parameter on the input rather than a
		// literal in the use case so that the day authentication exists, rows written
		// before it stay honestly labelled. See ADR-0021.
		ActorAuthenticated: false,
		Reason:             reason,
		RequestID:          RequestIDFromContext(r.Context()),
		TraceID:            traceIDFromContext(r.Context()),
	})
	if err != nil {
		s.writeReviewDecisionError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentReviewDecision(result, actor))
}

// reviewItemID reads and validates the {id} path value, writing the problem document
// itself when it cannot. ok is false when a response has been sent.
func (s *Server) reviewItemID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !reviewItemIDPattern.MatchString(id) {
		WriteProblem(w, r, InvalidParameter(r, "The review item id must look like rev_<identifier>."))
		return "", false
	}
	return id, true
}

// reviewActor reads the asserted reviewer identity, writing the problem document itself
// when it is absent or unusable. ok is false when a response has been sent.
//
// It is a 400 and not a 401. A 401 obliges the server to offer a WWW-Authenticate
// challenge, and there is no scheme to name: this header is not a credential and no
// value of it would authenticate anybody. Answering 401 would send a caller looking for
// a key that does not exist.
func (s *Server) reviewActor(w http.ResponseWriter, r *http.Request) (actor string, ok bool) {
	actor = strings.TrimSpace(r.Header.Get(HeaderReviewActor))
	if actor == "" {
		WriteProblem(w, r, InvalidParameter(r, reviewActorDetail))
		return "", false
	}
	if utf8.RuneCountInString(actor) > maxReviewActorLength {
		WriteProblem(w, r, InvalidParameter(r,
			"The reviewer name is too long. It is a name for an audit row, not a document."))
		return "", false
	}
	// The name is written verbatim into audit_events and read back by whatever later
	// prints the audit trail. A control character in it is a log-forging and
	// terminal-escape vector, so it is refused rather than stripped: a silently altered
	// name attributes a decision to somebody who does not exist under that spelling.
	if strings.ContainsFunc(actor, unicode.IsControl) {
		WriteProblem(w, r, InvalidParameter(r,
			"The reviewer name must not contain control characters. It is recorded verbatim in the audit trail."))
		return "", false
	}
	return actor, true
}

// reviewReason decodes and validates the decision body. ok is false when a response has
// been sent.
//
// Unknown fields are refused rather than ignored. A caller who sent {"resaon": "..."}
// has made a decision with no reason and would otherwise be told so by a validation
// error about the field they did spell correctly, which is the least useful possible
// message.
func (s *Server) reviewReason(w http.ResponseWriter, r *http.Request) (string, bool) {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxReviewDecisionBody+1))
	dec.DisallowUnknownFields()

	var body ReviewDecisionRequest
	if err := dec.Decode(&body); err != nil {
		WriteProblem(w, r, ValidationFailed(r,
			`The request body must be a JSON object of the form {"reason": "..."} and no larger than 8 KiB.`).WithCause(err))
		return "", false
	}
	// A second value in the stream is a body that decoded to something other than what
	// was sent, which is worth refusing for the same reason an unknown field is.
	if dec.More() {
		WriteProblem(w, r, ValidationFailed(r, "The request body must be a single JSON object."))
		return "", false
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		WriteProblem(w, r, ValidationFailed(r,
			"State a reason for the decision. A decision with no stated reason is not an audit trail, it is a timestamp."))
		return "", false
	}
	return reason, true
}

// noStore marks a response uncacheable. The review surface is a moderation queue whose
// contents change with every decision and whose rows are not public; nothing in the
// path between here and a reviewer should be storing any of it.
func (s *Server) noStore(w http.ResponseWriter) {
	w.Header().Set(HeaderCacheControl, "no-store")
}

// writeReviewError maps a read failure onto the error catalogue in api.md §6.
//
// domain.ErrValidation on the read side has two sources and they need different
// sentences: the use case rejects an unrecognised state, kind or SLA class rather than
// returning a silently empty page, and the repository rejects a cursor this API did not
// issue. Naming only the filters is what this did before, and a reviewer whose cursor
// was truncated in a chat message was then sent to fix three parameters that were
// correct -- with no way to page past it, because every retry produced the same advice
// about the same three values.
func (s *Server) writeReviewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		WriteProblem(w, r, NotFound(r, "No review item with that id was found."))
	case errors.Is(err, domain.ErrValidation):
		WriteProblem(w, r, InvalidParameter(r, reviewFilterDetail(r)).WithCause(err))
	default:
		WriteProblem(w, r, Internal(r, err))
	}
}

// reviewFilterDetail names the parameters that could actually have caused a validation
// failure on this request.
//
// It reads the request rather than the error text: the two failures are the same
// sentinel wrapped in different messages, and matching on a message is a test that
// passes until somebody rewords an error. What the request carried is a fact, so a
// caller who sent no cursor is never told to check one, and a caller who sent no
// enumerated filter is never sent to re-read the three vocabularies. The report for
// this phase asks the application for a distinct sentinel so that the remaining
// ambiguity -- a request carrying both -- can be resolved as well.
func reviewFilterDetail(r *http.Request) string {
	q := r.URL.Query()
	hasCursor := strings.TrimSpace(q.Get("cursor")) != ""
	hasFilters := len(q["state"])+len(q["kind"])+len(q["sla"]) > 0

	switch {
	case hasCursor && hasFilters:
		return "Either a filter value is not one this queue recognises -- check 'state', " +
			"'kind' and 'sla' -- or 'cursor' is not a cursor this API issued."
	case hasCursor:
		return "The 'cursor' parameter is not a cursor this API issued. Drop it to start " +
			"from the first page."
	default:
		return "A filter value is not one this queue recognises. Check 'state', 'kind' and 'sla'."
	}
}

// writeReviewDecisionError maps a decision failure onto the error catalogue in api.md §6.
//
// It differs from the read mapping in one branch, and the difference matters to a
// caller: on a decision, domain.ErrValidation is about the body, not about a query
// parameter, so it is validation-failed. An item somebody has already decided is
// domain.ErrConflict and 409 -- nothing the caller can fix by retrying or by editing
// their payload, which is exactly what a 400 would have implied.
func (s *Server) writeReviewDecisionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		WriteProblem(w, r, NotFound(r, "No review item with that id was found, or it is no longer open."))
	case errors.Is(err, domain.ErrConflict):
		WriteProblem(w, r, Conflict(r,
			"This review item has already been decided. Reload it to see who decided it and why.").WithCause(err))
	case errors.Is(err, domain.ErrValidation):
		WriteProblem(w, r, ValidationFailed(r,
			"The decision was refused: a reviewer name and a reason are both required.").WithCause(err))
	default:
		WriteProblem(w, r, Internal(r, err))
	}
}
