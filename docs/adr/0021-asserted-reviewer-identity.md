# ADR-0021: Review decisions record an asserted, unauthenticated actor

- **Status:** Accepted
- **Date:** 2026-09-05
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0007, ADR-0019, ADR-0020

## Context

The review queue is the mechanism by which a candidate that failed a validation gate can still become a published release. Blueprint §16 requires that every such decision carry "the audit trail of who approved it", and `audit_events` has existed since the initial migration with exactly the right columns — actor type, actor id, action, subject, before and after state, reason, request id — and nothing has ever written a row to it.

FirmScout has no login. There is no session, no user table, no identity provider, and SSO is explicitly out of scope for this phase. The queue, meanwhile, is unusable today: items accumulate and nothing can resolve them. So the question is not "how should reviewers authenticate" but "what does the audit trail honestly say about who decided, given that the platform cannot know".

There is a related deployment question. The endpoints that perform these decisions write to the catalogue — they publish releases and reject candidates. An unauthenticated write endpoint reachable from the public internet is not a review queue, it is a defacement API.

## Decision

**The actor on a review decision is a string the caller asserted, the platform did not verify it, and the schema records that fact rather than hiding it.**

Concretely:

- Every decision carries an actor supplied by the caller — the `X-FirmScout-Actor` request header on the internal HTTP surface, or the `--actor` flag on the CLI's `review accept`/`review reject` (added in the amendment below). A blank actor is refused, as is a blank reason: a decision with no stated reason is not an audit trail, it is a timestamp.
- `audit_events` gains `actor_authenticated BOOLEAN NOT NULL DEFAULT false`. Every row this phase writes sets it `false`. It is a parameter on `ReviewDecisionInput` rather than a hardcoded literal so that the day authentication exists, rows written before it stay honestly labelled instead of being retro-interpreted as verified.
- The review endpoints live under `/internal/`, not `/api/v1/`, and are **not registered at all** unless the server is built with the review dependencies *and* `Deps.ReviewAPIEnabled` is true. Two independent switches, defaulting off, because this surface performs unauthenticated writes and a single accidental wiring should not be enough to expose it. *(A third switch, `FIRMSCOUT_REVIEW_UI_ENABLED` in `apps/web`, also bears on this surface and was not named here until the amendment below — that omission is the reason the amendment exists.)*
- `docs/architecture/api.md` states the deployment constraint plainly: this surface must not be exposed to the public internet; it belongs behind an authenticating proxy or on a private network. The platform does not pretend otherwise.
- The internal surface is deliberately absent from `docs/api/openapi.yaml`, which is the machine-readable *public* contract. A client generator pointed at it must not produce bindings for an off-by-default unauthenticated surface.

## Consequences

### Positive

- The queue becomes usable now, which is the difference between a review mechanism and a table that fills up.
- The audit trail is honest at the level of a column, not a comment. A future reader — or a future version of this system, after authentication exists — can separate "a person we verified decided this" from "somebody claiming to be Alex decided this" with a `WHERE` clause rather than by knowing which migration shipped when.
- Adding authentication later is additive: the column already exists, the parameter already exists, and the rows already say `false`. Nothing has to be back-filled with a guess.
- The default-off double switch means the dangerous surface does not exist in a default build. A deployment has to opt in twice, and the environment variable that does it (`FIRMSCOUT_REVIEW_API_ENABLED`) defaults to false. (As the amendment below records, this reasoning was incomplete: it protected the HTTP surface from being reached directly, and said nothing about a public first-party client reaching it *for* a visitor.)
- A human override of a gate is recorded in three places that must agree: the gate verdicts stay in `validation_results`, the review item records who resolved it and why, and `audit_events` records the same decision with the reason and request id. An override is never silent.

### Negative

- **The audit trail can be forged by anyone who can reach the endpoint.** A caller can put any name in the header, including somebody else's. `actor_authenticated = false` is a truthful label on that, not a mitigation: it tells a reader the name is unverified, it does not make the name harder to fake. Until authentication exists, the operational control is network placement, and network placement is a thing operators get wrong.
- This is exactly the kind of decision that becomes permanent because it works. A queue that is usable is a queue nobody urgently fixes, and "we will add SSO later" is a sentence with a poor track record. The `Revisit when` section below is the only thing scheduled against it.
- Two switches protecting one surface means two places to get the wiring wrong in the other direction: an operator who enables the flag but does not place the service behind a proxy has an open write endpoint and no error message telling them so. Nothing in the code can detect its own network exposure. **This is exactly what happened, from a third direction this bullet did not anticipate: see the amendment below.** The API was placed correctly, on a private subnet; the wiring that went wrong was a public web app calling the API on a visitor's behalf, which functioned as an open write endpoint without the API itself ever being reachable from outside its private subnet.
- The internal surface is documented in `api.md` but absent from the OpenAPI document, so the two files no longer describe the same set of endpoints. That is deliberate, but it means "the OpenAPI file is the contract" is now true only of the public API, and a reader has to know that.
- Recording a self-asserted name may create a false sense of accountability for the people using it — a name in a log looks like attribution whether or not anything checked it.

### Neutral

- No user table, no roles, no permissions. Every actor who can reach the endpoint can do everything the endpoint offers; there is no reviewer-versus-approver distinction to model yet.
- The system actor for decisions the pipeline makes on its own — closing a conflict because the sources came back into agreement — is the literal string `system`, recorded with the same `actor_authenticated = false`, because the pipeline is not a verified identity either.

## Amendment (2026-09-05): the public web app must not perform the write

This ADR shipped saying network placement was "the operational control" for the write endpoints (Negative, above), and that a public deployment with the surface itself enabled and reachable "is a defect" (Revisit when, below). It did not consider a second way to reach the endpoints publicly without the endpoints themselves ever being exposed: **a public deputy that calls them on a visitor's behalf.**

`apps/web` — the public catalogue site, deployed to the internet per ADR-0007 — added a review-queue viewer and, alongside it, `lib/review.ts#decideReviewItem`, a function that ran *inside apps/web's own server* and issued the `POST /internal/review/items/{id}/{accept,reject}` requests itself, from a Next.js Server Action bound to the page's Accept/Reject buttons. Both halves of the deployment followed their documented story exactly — the API server on a private subnet with `FIRMSCOUT_REVIEW_API_ENABLED=true`, the public site with its own `FIRMSCOUT_REVIEW_UI_ENABLED=true` — and the result was still an internet-reachable, unauthenticated publish. `apps/web`'s server necessarily sits somewhere that can reach the API (that is what makes the proxy work at all), and nothing about that placement is what api.md's "must not be exposed to the public internet" was written to describe: it describes the API server, not a second, public server that calls it. As this ADR already said, "nothing in the code can detect its own network exposure" — the gap was that the public web app *was* the network exposure, and no switch here named it.

`FIRMSCOUT_REVIEW_UI_ENABLED` had existed in `apps/web/lib/review.ts` and `apps/web/app/review/page.tsx` since the review UI was first built, but this ADR, `api.md` §11 and the consistency report all described exactly two switches — both on the API server — and none of the three mentioned that a first-party client could reach the same endpoints unattended. That is corrected here: **there have always been three switches bearing on this surface, and this is the first document to name all of them.**

### The fix adopted

`apps/web` loses its write path entirely, rather than gaining a stronger warning. `decideReviewItem`, the `/review` Server Action (`acceptReviewItem`/`rejectReviewItem`), and the accept/reject form are deleted from the web app's source — a banner was the old design's only control, and a banner is not a control an operator setting environment variables ever reads. `FIRMSCOUT_REVIEW_UI_ENABLED` now gates a **read-only** queue viewer only: the queue list and one item's detail, including its evidence, gate verdicts and audit trail, exactly as before. It cannot gate a write that no longer exists in that codebase to gate.

Accepting or rejecting an item is now the CLI's job alone: `firmscout review accept --id <id> --actor <name> --reason <text>` and the equivalent `review reject` (`apps/cli/main.go`). The CLI does not reach this by calling the HTTP surface — it calls `application.DecideReviewItem` directly, the same use case the HTTP handler calls, against the database its `FIRMSCOUT_DATABASE_URL` names. This adopts, in narrowed form, the "make the endpoints public but read-only, and perform decisions through the CLI only" alternative recorded below: it does not touch the *HTTP* endpoints — removing or changing those is outside what this document or `apps/web` controls — it makes **FirmScout's own public web app** structurally unable to write, and gives the CLI a decision path that has never depended on the HTTP review surface being reachable, or even running.

### Residual risk, honestly

- The `POST /internal/review/items/{id}/{accept,reject}` endpoints described in `api.md` §11.1 are unchanged: they still exist, are still unauthenticated, and still rely entirely on network placement. What changed is that no FirmScout-authored client calls them from a public deployment any more. Whether some other internal tool still does is outside what this amendment can verify; if nothing does, that surface's owner should weigh removing it now that the CLI no longer needs it either.
- The review pages remaining in `apps/web` are read-only but still unauthenticated: anyone who can reach `/review` on a public deployment sees candidate provenance, evidence excerpts, priority scoring, and the audit trail's asserted (unverified) reviewer names. That is real information disclosure — smaller than a forged publish, not zero. This ADR's "must not be exposed to the public internet" guidance is hereby extended to cover `FIRMSCOUT_REVIEW_UI_ENABLED`, not only the two switches `api.md` §11 names today; that document should be updated to say so by whoever owns it.
- The CLI's control is that running it requires a database connection string and a shell on a host that has one. That is a genuinely different boundary from "reachable HTTP endpoint on a private subnet" — a leaked connection string is at least as dangerous as a leaked network path — but it is the same boundary operators already protect for every other command this CLI runs (`migrate up`, `registry sync`), not a bespoke new one invented for review decisions.

### Follow-through (2026-09-05)

The last bullet above asked the owner of `docs/architecture/api.md` to extend its "must not be exposed to the public internet" constraint to `FIRMSCOUT_REVIEW_UI_ENABLED`. That has been done: §11 of that document now opens on all three switches, gives the host each is set on and what each gates, states that the web switch is an information-disclosure risk rather than a harmless one, and records that network placement is the only real control. `docs/architecture/security.md` gained the corresponding threat (T-16) and §4.11. This note exists so a later reader can tell that the request was carried out rather than merely written down; the bullet itself is left as it was, because an ADR records what was decided at the time.

Two documents this ADR does not own still described "two independent switches" and were flagged for their owners: `docs/diagrams/api-sequence.md` (the bullet under the sequence diagram) and `docs/architecture/consistency-report.md` (the reviewer-surface row). **Both were corrected in the closing pass** — the diagram's bullet now names three switches on two hosts and says which one mattered, and the report's §4 row does the same and cross-references the two new entries that record the defect itself (§9's closed list and §10's security block). Three Go and TypeScript comments that repeated the undercount were corrected with them: `internal/adapters/httpapi/router.go` (twice), `internal/adapters/httpapi/review_handlers.go` and `apps/web/app/review/page.tsx`.

One correction to the framing above, which the closing pass verified rather than assumed: none of this was ever deployed. The review surface, this ADR, and the write path it describes are all uncommitted Phase-2 work in a project that has never built an image or run a container. "Both halves of the deployment followed their documented story" describes the deployment that would have happened, not one that did. That does not reduce the finding — no user was exposed, and nothing in the documented deployment story would have prevented it — but the tense should not be read as a breach report.

## Alternatives considered

### A shared secret in a header

Rejected. It is authentication built badly: one secret for every reviewer, no revocation short of rotating it everywhere, and no way to tell two reviewers apart in the audit trail — which is the entire point of the exercise. A static secret in an environment variable also has a strong tendency to end up in a compose file in the repository, at which point it is neither secret nor authentication, only ceremony.

### Record `system` as the actor for every decision

Rejected outright. The one column the audit trail exists to fill is "who", and filling it with a value known to be wrong makes the table worse than empty: an empty table is obviously incomplete, whereas a table that says `system` decided everything looks complete and is false.

### Block the review queue until SSO exists

Rejected. SSO is out of scope for this phase and the queue is unusable today — candidates that fail a gate accumulate with nothing able to resolve them, which means the conflict detection built in ADR-0020 produces findings nobody can act on. Waiting would trade a real, bounded honesty problem for a real, unbounded correctness one.

### Make the endpoints public but read-only, and perform decisions through the CLI only

Originally rejected for this phase, on the grounds that it would cost shell access to the deployment for every decision, which does not scale past the founding team and pushes the same identity question onto SSH key management. The note that follows it — "it remains the fallback if the internal HTTP surface proves hard to place safely" — is exactly what happened: see the amendment above. **Adopted, in narrowed form, on 2026-09-05:** not by making the HTTP endpoints themselves read-only (they are unchanged), but by removing the write path from `apps/web` specifically and giving the CLI a decision path that talks to the database directly rather than to the HTTP surface. The scaling cost this alternative was rejected for is real and unresolved — it returns as the first bullet under Revisit when, below.

## Revisit when

- More than the founding team needs to make review decisions. The CLI-only path adopted in the amendment above trades a public UI for requiring a database connection string and a shell, which does not scale past a handful of trusted operators — this is the cost the CLI-only alternative was originally rejected for, now accepted deliberately but not indefinitely.
- FirmScout has any authentication mechanism at all — an identity provider, an admin session, or API keys with an admin scope — at which point `actor_authenticated` starts being written `true` for verified callers, the pre-authentication rows keep meaning what they say, and a public decision UI in `apps/web` becomes possible to reconsider on its own terms rather than reintroducing this ADR's original defect.
- More than the founding team reviews items (read-only), which is the point at which "everyone who can reach it can do anything" stops being an accurate description of the trust boundary for the queue viewer, even without a write path.
- The hosted service launches. A public deployment with the HTTP write endpoints enabled and reachable, or with `FIRMSCOUT_REVIEW_UI_ENABLED` on for a build that still contains a write path, is a defect, and launch is the moment to verify it is neither.
- A reviewer needs a permission distinction — someone who can reject but not publish — which this model cannot express at all.
