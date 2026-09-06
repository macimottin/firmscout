# ADR-0022: One canonical problem-type catalogue, taken from api.md

- **Status:** Accepted
- **Date:** 2026-09-05
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0014, ADR-0019, ADR-0021

## Context

Every error this API returns is an RFC 9457 `application/problem+json` document, and the `type` member is a URI a consumer branches on. Two places in the repository declared what those URIs are, and they disagreed.

`docs/architecture/api.md` §6 catalogued ten types. `internal/adapters/httpapi/problem.go` declared eight, three of them under different slugs — `invalid-request` where the document said `invalid-parameter`, `unauthenticated` where it said `unauthorized`, `internal` where it said `internal-error` — and it was missing `validation-failed` and `invalid-api-key` entirely. The divergence was known: the file carried a doc comment naming it and saying the two "must be reconciled before the API is published", deliberately refusing to resolve it silently in code because which set wins is a contract decision.

Phase 2 forces the question, because the review endpoints need a 409 and there is no type for one. Emitting an existing 400 for "this item has already been decided" would tell a caller their request was malformed, which is false and sends them to edit a payload that was correct.

The API is pre-alpha. It is unpublished, there are no keyed consumers, and the only in-repo client is `apps/web`, whose fetch layer branches on HTTP status rather than on `type`. Nobody is depending on either spelling today.

## Decision

**The catalogue in `docs/architecture/api.md` §6 is canonical. The Go code moves to it, and two types are added.**

Three reasons, in order of weight:

1. **`api.md`'s preamble already declared itself the tie-break** — "the source of truth when the two could be read as disagreeing" — and it did so before this particular disagreement existed. A rule written before the tie is worth more than one invented afterwards to justify whichever side is cheaper to change.
2. **The documented set is the more informative one.** `invalid-parameter` versus `validation-failed` tells a consumer whether to fix a query string or a request body. `unauthorized` versus `invalid-api-key` tells them whether to send a credential or replace one. Both are branches a client library genuinely writes, and a single blanket type collapses them into "something about auth went wrong", which is where a consumer starts guessing.
3. **A problem type is an identifier consumers hardcode**, so the cost of renaming one only ever goes up. Pre-alpha with zero keyed consumers is the cheapest this will ever be.

The canonical set, after reconciliation, is the eleven types in api.md §6: `not-found`, `invalid-parameter`, `validation-failed`, `unauthorized`, `invalid-api-key`, `forbidden`, `conflict`, `rate-limited`, `quota-exceeded`, `internal-error`, `service-unavailable`.

**Renames** (code → canonical): `invalid-request` → `invalid-parameter`; `unauthenticated` → `unauthorized`; `internal` → `internal-error`.

**Additions:** `invalid-api-key`, split out of the old blanket 401 so a revoked credential is distinguishable from an absent one; and `conflict` (409), for an operation the resource's state forbids.

The Go identifiers change with the values (`TypeInvalidRequest` → `TypeInvalidParameter`, `TypeUnauthenticated` → `TypeUnauthorized`, and the constructor functions with them), so a stale reference is a compile error rather than a runtime surprise. `TypeInternal` keeps its name and changes its value, because "internal" is still what the constant is about.

The 401 split lands in the `APIKey` middleware as four distinct branches: no key store configured → `unauthorized`; a present but unparseable `Authorization` header → `unauthorized`; a well-formed token that fails resolution with `ErrNotFound` → `invalid-api-key`; the key store unreachable → `service-unavailable`, unchanged.

`api.md` §6 gains a short "Reconciled in Phase 2" table recording the old spellings, so a reader of an earlier draft can map them, and `docs/api/openapi.yaml` declares the eleven URIs as a closed `enum` on the `Problem` schema's `type` member. A test asserts every Go constant against the documented URI character for character.

## Consequences

### Positive

- One catalogue, in two files that a test keeps in agreement. The `problem.go` doc comment that documented a known divergence is replaced by one that documents a resolved one.
- The review endpoints can answer 409 truthfully. An already-decided item is a fact about the resource, not a defect in the request.
- A consumer can distinguish "your key is dead" from "you sent no key", and "your query string is wrong" from "your body is wrong", each of which is a different fix.
- The OpenAPI `enum` makes the set closed and machine-checkable, so a new type cannot be introduced in code without the contract document noticing.
- Doing it now costs one commit. Doing it after the first keyed consumer costs a version bump and a deprecation window (api.md §8).

### Negative

- **Three public identifiers changed value.** The claim that nobody depended on them is true today and unverifiable in general: a draft of the old catalogue may exist in somebody's notes, and anyone who read the pre-Phase-2 code and hardcoded `invalid-request` gets a URI that now never appears. The migration table in api.md §6 is the whole mitigation, and it is documentation, not a redirect.
- Two 400s and two 401s mean a consumer writing exhaustive error handling has four branches where they had two. That is more informative and it is also more work, and a consumer who only wants "did it fail" now has to know that two types share a status.
- The split leaks a small amount of information to an unauthenticated caller: `invalid-api-key` confirms that a credential was presented and rejected, where a blanket `unauthorized` would not have said which. This is judged acceptable — the caller already knows what they sent, and the alternative penalises every legitimate consumer debugging a rotated key.
- `TypeInternal` now holds `internal-error`, so the constant name and its value differ by a suffix. A reader skimming the constant block can misread one for the other.

### Neutral

- No status code changed for any existing situation. Only the `type` URI and, in one case, which of two 401s is emitted.
- `validation-failed` was already in the documented catalogue attached to `POST /lookup`, which is out of scope for this phase. It is emitted now by the review decision endpoints' body validation instead, so the type stops being aspirational.

## Alternatives considered

### Keep the code's spellings and rewrite api.md

Rejected. It is the cheaper edit — one document instead of a package and its tests — and that is close to the whole of its case. It would also mean overruling the tie-break rule `api.md` states about itself the first time that rule was ever needed, which makes the rule worth nothing afterwards. And it would keep the less informative set: one 401 for two situations, one 400 for two, and no 409 at all, which the review endpoints need.

### Keep both spellings, with the old URIs as aliases

Rejected. An alias set is two catalogues with a promise attached, and the promise is the part that rots: nothing would stop a handler emitting the deprecated URI, and a consumer branching on either would be correct, so the duplication would never end. RFC 9457 offers no aliasing mechanism, so this would be FirmScout inventing one for a problem it does not have — there are no consumers to migrate.

### Defer the reconciliation until the API is published

Rejected. Publication is exactly the moment the change stops being free. Deferring also means shipping the review endpoints with no 409, which would mean answering "already decided" with a type that says the request was malformed — trading a documentation inconsistency for a response that is actively wrong.

### Drop the type URIs and branch on status codes alone

Rejected, and it is further from the mark than it looks. Two of the statuses in the catalogue carry two meanings each: 429 is either a short-window bucket or a monthly quota, and 401 is either a missing credential or a dead one. The `type` member is the only thing that separates them, and RFC 9457 exists because status codes ran out of room for exactly this.

## Revisit when

- The first external consumer registers an API key. From that point a change to this catalogue is a breaking change under api.md §8 and needs `/api/v2`, a `Deprecation` header, and a 90-day notice.
- A situation arises that none of the eleven types describes. The right response is a new type in api.md §6 first and the Go constant second, never the other way round — the test that pins them together will say so.
- `POST /api/v1/lookup` ships, which is the endpoint `validation-failed` was originally written for and the first place a body-validation error reaches a paying consumer.
- Authentication for the internal review surface arrives (ADR-0021), which would add a real 401 path there and may make `forbidden` meaningful for a reviewer role.
