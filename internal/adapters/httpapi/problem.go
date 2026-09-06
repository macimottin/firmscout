package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// ProblemBase is the stable prefix every FirmScout problem type shares. The URIs are
// identifiers first and documentation second: RFC 9457 does not require them to
// resolve, but they are intended to.
const ProblemBase = "https://firmscout.dev/problems/"

// The problem catalogue. A consumer branches on these values, so they are part of the
// public contract and may never be respelled without a version bump.
//
// # Reconciled in Phase 2
//
// This package and docs/architecture/api.md §6 used to catalogue different slugs for
// the same five situations. The document won, because its own preamble already declared
// itself the tie-break, and because its split is the more informative one:
// invalid-parameter versus validation-failed tells a consumer whether to fix a query
// string or a body, and unauthorized versus invalid-api-key tells them whether to send
// a credential or replace one. Both are branches a client library genuinely writes.
//
// Three URIs changed value -- invalid-request became invalid-parameter, unauthenticated
// became unauthorized, internal became internal-error -- and the Go identifiers changed
// with two of them, so a stale reference is a compile error rather than a runtime
// surprise. The API is pre-alpha, unpublished and has no keyed consumers; this was the
// last cheap moment. See ADR-0022.
const (
	// TypeNotFound: a slug or id does not resolve to an existing record.
	TypeNotFound = ProblemBase + "not-found"
	// TypeInvalidParameter: a query or path parameter failed validation, or a
	// required non-credential header is absent.
	TypeInvalidParameter = ProblemBase + "invalid-parameter"
	// TypeValidationFailed: a request body is malformed or fails its rules. It is a
	// separate type from TypeInvalidParameter, and both are 400, because the two send
	// a consumer to different places: one to the query string they built, the other
	// to the JSON they serialised.
	TypeValidationFailed = ProblemBase + "validation-failed"
	// TypeRateLimited: the short-window token bucket for this caller is exhausted.
	TypeRateLimited = ProblemBase + "rate-limited"
	// TypeQuotaExceeded: the durable monthly quota for this key is exhausted. It is
	// a separate type from TypeRateLimited even though both are 429, because the two
	// need entirely different responses from the consumer: back off for seconds, or
	// buy more quota.
	TypeQuotaExceeded = ProblemBase + "quota-exceeded"
	// TypeUnauthorized: an endpoint requiring a key received none, or an unparseable
	// Authorization header, or the deployment cannot check keys at all.
	TypeUnauthorized = ProblemBase + "unauthorized"
	// TypeInvalidAPIKey: a well-formed credential was presented and does not resolve
	// to a live, non-revoked key. Split out of the old blanket 401 because "send a
	// credential" and "replace the one you sent" are different instructions, and a
	// consumer that cannot tell them apart retries the same dead key forever.
	TypeInvalidAPIKey = ProblemBase + "invalid-api-key"
	// TypeForbidden: an authenticated caller's plan does not reach this endpoint.
	TypeForbidden = ProblemBase + "forbidden"
	// TypeConflict: the state of the resource forbids the operation -- a review item
	// somebody has already decided. Without it the review endpoints would have to
	// report an already-decided item as a malformed request, which is a lie that
	// sends the caller to fix their own payload.
	TypeConflict = ProblemBase + "conflict"
	// TypeInternal: an unhandled server-side failure. Its detail is always generic.
	// The identifier keeps its name and changes its value; see the note above.
	TypeInternal = ProblemBase + "internal-error"
	// TypeServiceUnavailable: a readiness gate is failing, e.g. the database is
	// unreachable. Distinct from TypeInternal because it is expected to be transient
	// and is safe for a client to retry.
	TypeServiceUnavailable = ProblemBase + "service-unavailable"
)

// Titles paired with the types above. RFC 9457 says the title should not change from
// occurrence to occurrence for the same type, which is why they are constants and not
// composed at the call site.
const (
	titleNotFound           = "Resource not found"
	titleInvalidParameter   = "Invalid parameter"
	titleValidationFailed   = "Request body validation failed"
	titleRateLimited        = "Too many requests"
	titleQuotaExceeded      = "Monthly quota exceeded"
	titleUnauthorized       = "Authentication required"
	titleInvalidAPIKey      = "API key invalid or revoked"
	titleForbidden          = "Not available on this plan"
	titleConflict           = "Conflicting state"
	titleInternal           = "Internal server error"
	titleServiceUnavailable = "Service temporarily unavailable"
)

// ProblemContentType is the media type RFC 9457 requires. Serving a problem document
// as application/json is a common and consequential mistake: a consumer's error
// handling keys off the media type to decide whether the body is a problem document
// at all.
const ProblemContentType = "application/problem+json"

// Problem is an RFC 9457 problem document plus FirmScout's requestId extension member.
//
// The cause field is unexported and never serialised. That is the whole point of the
// type: the value that reaches the client is assembled from a fixed catalogue, while
// the underlying error -- which may name a table, a host, a driver or a query -- goes
// only to the server-side log, correlated to the client's copy by requestId.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	RequestID string `json:"requestId,omitempty"`

	cause error
}

// Error makes a Problem usable as an error value inside the adapter.
func (p Problem) Error() string { return p.Title }

// Unwrap exposes the internal cause to errors.Is/As on the server side only.
func (p Problem) Unwrap() error { return p.cause }

// WithCause attaches the internal error. It is logged, never served.
func (p Problem) WithCause(err error) Problem {
	p.cause = err
	return p
}

// WithDetail replaces the human-readable detail. The caller is responsible for the
// detail being safe to show a stranger: it is written by FirmScout, never derived from
// an error string.
func (p Problem) WithDetail(detail string) Problem {
	p.Detail = detail
	return p
}

// NotFound builds a 404 problem for r.
func NotFound(r *http.Request, detail string) Problem {
	return newProblem(r, TypeNotFound, titleNotFound, http.StatusNotFound, detail)
}

// InvalidParameter builds a 400 problem for a query or path parameter, or for a
// required header that is not a credential.
func InvalidParameter(r *http.Request, detail string) Problem {
	return newProblem(r, TypeInvalidParameter, titleInvalidParameter, http.StatusBadRequest, detail)
}

// ValidationFailed builds a 400 problem for a request body that is malformed or breaks
// its own rules.
func ValidationFailed(r *http.Request, detail string) Problem {
	return newProblem(r, TypeValidationFailed, titleValidationFailed, http.StatusBadRequest, detail)
}

// RateLimited builds a 429 problem for the short-window bucket.
func RateLimited(r *http.Request, detail string) Problem {
	return newProblem(r, TypeRateLimited, titleRateLimited, http.StatusTooManyRequests, detail)
}

// QuotaExceeded builds a 429 problem for the durable monthly quota.
func QuotaExceeded(r *http.Request, detail string) Problem {
	return newProblem(r, TypeQuotaExceeded, titleQuotaExceeded, http.StatusTooManyRequests, detail)
}

// Unauthorized builds a 401 problem for a caller who presented no usable credential,
// or for a deployment that cannot check one.
func Unauthorized(r *http.Request, detail string) Problem {
	return newProblem(r, TypeUnauthorized, titleUnauthorized, http.StatusUnauthorized, detail)
}

// InvalidAPIKey builds a 401 problem for a credential that was presented and does not
// resolve.
func InvalidAPIKey(r *http.Request, detail string) Problem {
	return newProblem(r, TypeInvalidAPIKey, titleInvalidAPIKey, http.StatusUnauthorized, detail)
}

// Forbidden builds a 403 problem.
func Forbidden(r *http.Request, detail string) Problem {
	return newProblem(r, TypeForbidden, titleForbidden, http.StatusForbidden, detail)
}

// Conflict builds a 409 problem for an operation the resource's current state forbids.
func Conflict(r *http.Request, detail string) Problem {
	return newProblem(r, TypeConflict, titleConflict, http.StatusConflict, detail)
}

// Internal builds a 500 problem whose detail is deliberately generic and whose cause
// is kept for the server-side log only.
func Internal(r *http.Request, cause error) Problem {
	p := newProblem(r, TypeInternal, titleInternal, http.StatusInternalServerError,
		"The request could not be completed. Quote the request id when reporting this.")
	p.cause = cause
	return p
}

// ServiceUnavailable builds a 503 problem for a failing readiness gate.
func ServiceUnavailable(r *http.Request, detail string, cause error) Problem {
	p := newProblem(r, TypeServiceUnavailable, titleServiceUnavailable, http.StatusServiceUnavailable, detail)
	p.cause = cause
	return p
}

func newProblem(r *http.Request, typ, title string, status int, detail string) Problem {
	p := Problem{Type: typ, Title: title, Status: status, Detail: detail}
	if r != nil {
		p.Instance = r.URL.Path
		p.RequestID = RequestIDFromContext(r.Context())
	}
	return p
}

// WriteProblem serialises p as application/problem+json, echoes the request id, and
// logs the internal cause server-side.
//
// It is the only way an error leaves this package. A handler that wrote
// http.Error(w, err.Error(), 500) would leak a driver message, a host name or a query
// fragment to a stranger; funnelling every failure through here makes that impossible
// by construction rather than by review.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	if p.RequestID == "" {
		p.RequestID = RequestIDFromContext(ctx)
	}
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	if p.Type == "" {
		p.Type = TypeInternal
		p.Title = titleInternal
	}

	logProblem(ctx, r, p)

	h := w.Header()
	h.Set("Content-Type", ProblemContentType)
	// A problem document is a per-request fact; caching one would be actively
	// harmful for the 429s and 503s that are meant to be retried.
	h.Set("Cache-Control", "no-store")
	if p.RequestID != "" {
		h.Set(HeaderRequestID, p.RequestID)
	}
	w.WriteHeader(p.Status)

	body, err := json.Marshal(p)
	if err != nil {
		// Marshalling a struct of strings and an int cannot fail; if it somehow
		// does, the status line has already been written and there is nothing
		// useful left to say.
		return
	}
	_, _ = w.Write(body)
}

func logProblem(ctx context.Context, r *http.Request, p Problem) {
	log := LoggerFromContext(ctx)
	attrs := []any{
		slog.String("problem_type", p.Type),
		slog.Int("status", p.Status),
	}
	if r != nil {
		attrs = append(attrs,
			slog.String("method", r.Method),
			slog.String("route", RoutePatternFromContext(ctx)),
		)
	}
	if p.cause != nil {
		// The cause is logged here and only here. It never enters the response body.
		attrs = append(attrs, slog.String("error", p.cause.Error()))
	}
	switch {
	case p.Status >= 500:
		log.ErrorContext(ctx, "request failed", attrs...)
	case p.Status == http.StatusTooManyRequests:
		log.WarnContext(ctx, "request throttled", attrs...)
	default:
		log.DebugContext(ctx, "request rejected", attrs...)
	}
}
