# Governance

This document describes how decisions get made on the FirmScout open-source project. It does not describe how the hosted service is run as a business — that's deliberate, and §5 explains the boundary explicitly.

FirmScout is young: at the time of writing there is one maintainer (@macimottin) and no released version. This document describes the model the project intends to operate under as it grows, not a claim that all of these roles are currently filled by different people.

## 1. Roles

| Role | Who | Can do |
| --- | --- | --- |
| **User** | Anyone running FirmScout, self-hosted or via the hosted service | Use the product, file issues, participate in discussions |
| **Contributor** | Anyone with a merged pull request | Everything a user can, plus: their name is in the project's history and they may be asked for input on changes touching their prior work |
| **Collector maintainer** | A contributor who has taken ongoing responsibility for one or more vendors' sources and collectors | Reviews and approves PRs touching "their" vendor's dataset entries, sources, and collectors; first point of contact when that vendor's site breaks |
| **Maintainer** | A contributor trusted with repository write access | Reviews and merges PRs across the project, triages issues, makes routine decisions, can propose and vote on ADRs |
| **Steering** | The role responsible for the project's overall direction, licensing, and trademark | Breaks ties, has final say on contested decisions and on anything with legal exposure (licensing, trademark, the compliance policy in §34 of the blueprint) |

A person can hold more than one role — a collector maintainer who consistently makes good decisions across the project is a natural candidate for maintainer, and a maintainer typically also acts as a collector maintainer for whatever they registered.

## 2. How decisions are made

FirmScout uses three decision modes, chosen by how much the decision costs to get wrong:

- **Lazy consensus, for routine changes.** A pull request that adds a vendor, fixes a bug, corrects a fact, or improves documentation needs one maintainer approval and no objection within a reasonable review window. Silence is consent. This is the default and covers the overwhelming majority of contributions.
- **An ADR (Architecture Decision Record), for architectural changes.** A change to how the system is structured — a new port, a change to the dependency rules, a new stateful dependency, a change to how publication works — needs a written ADR before it's implemented, not after. See §3.
- **A maintainer vote, for contested decisions.** When a change is proposed and a maintainer objects and the disagreement doesn't resolve through discussion, it goes to a vote among maintainers. Simple majority, steering breaks ties. This is meant to be rare — if votes are happening often, that's a sign discussion is failing earlier in the process, not a sign the process is working.

## 3. How ADRs work

An ADR is required whenever a change would alter something in the blueprint's [architecture decision summary](docs/architecture/blueprint.md#6-architecture-decision-summary) or add a new entry to it — a new stateful dependency, a change to the layer boundaries, a change to the licensing model, a change to how AI is allowed to touch the system, and so on. As a rule of thumb: if reversing the decision later would mean rewriting more than one bounded context, it needs an ADR first.

Each ADR follows the existing format in [`docs/adr/`](docs/adr/): Status, Context, Decision, Consequences, Alternatives considered, and whether it requires legal review. A proposed ADR is itself a pull request; it needs the same lazy-consensus-or-vote treatment as any other contested change, and it should be merged (as `Accepted`, `Rejected`, or `Superseded`) before, not after, the code that depends on it.

An ADR that's superseded stays in the repository with its status updated — the record of "we tried this and changed our minds" is as valuable as the current decision.

## 4. Becoming a maintainer, and stepping down

**Becoming a maintainer** is by nomination from an existing maintainer, based on a track record: multiple merged, well-reviewed contributions; sound judgment shown in issue and PR discussion, particularly around the compliance and evidence rules that are easy to erode under time pressure; and a demonstrated understanding of the architecture boundaries this project cares about. There's no fixed contribution count or tenure requirement — it's a judgment call, made in the open, with other maintainers given the chance to object before it's finalized.

**Stepping down** requires nothing more than saying so. Maintainers are volunteers (even the ones who also work on the hosted service); life gets in the way, priorities change, and there's no obligation to explain why. A maintainer who has been inactive for an extended period without stepping down may be moved to an emeritus/inactive state by steering, so that "who can approve this" stays an accurate list rather than an aspirational one. Returning is always welcome.

## 5. The project and the hosted service are separate

This needs to be stated plainly, because the confusion is common and corrosive to trust: **the FirmScout open-source project and the FirmScout hosted service are governed separately.** The hosted service is a business built on top of the open-source platform, per the three-part model in the [README](README.md); it does not get to run the open-source project as if it were the same thing.

Concretely, the hosted service **does not get to impose**, unilaterally, on the open-source project:

- What gets merged into `main`, or the review bar for a contribution.
- Changes to the source code licence (Apache-2.0) or to the dataset licence in a way that reduces rights the community already has.
- Which sources or vendors are supported in the open-source registry — the hosted service may choose to enable/disable sources for its own operational reasons, but that's an operational choice on its infrastructure, not a change to the shared `dataset/` registry.
- Architectural decisions, which follow the ADR process in §3 regardless of who's asking.
- The Code of Conduct or how it's enforced.

What the hosted service *can* reasonably do: fund maintainer time, sponsor infrastructure (CI, the observability stack, legal review of the licensing questions in the blueprint), and propose changes through the same process as any other contributor — including, notably, the same scrutiny on a proposal that happens to also benefit the hosted service's business model. A maintainer employed by the entity operating the hosted service is still a maintainer bound by this document, not an exception to it.

## 6. Dataset stewardship

The dataset (`dataset/`, `collectors/config/`) is curated data under code review, per [ADR-0016](docs/adr/0016-hybrid-dataset.md). Its accuracy is the project's core promise, so its stewardship is deliberately conservative:

- **Additions** (a new vendor, product, source, or collector config) follow ordinary lazy consensus, gated by schema validation and, for sources, the compliance review in [DATA_SOURCES.md](DATA_SOURCES.md).
- **Corrections to a published fact** follow [`docs/collectors/dataset-correction-policy.md`](docs/collectors/dataset-correction-policy.md) and require evidence, not just an assertion that something is wrong.
- **Approval authority for dataset corrections** sits with the relevant collector maintainer where one exists for that vendor, or any maintainer otherwise. A correction that reverses a previous maintainer's published judgment, or that's contested, escalates to a maintainer vote per §2.
- **Registry rows are never hand-edited in the database.** `firmscout registry sync` is the only writer of registry tables; anyone who needs a database-level fix has found a bug in the sync process, not a shortcut around review.

## 7. Conflicts of interest

Contributors employed by, or otherwise financially connected to, a vendor catalogued in FirmScout (MikroTik, Fortinet, Poly/HP, Dell, or any future vendor) are welcome and, honestly, likely to be some of the most valuable contributors the project has — nobody understands a vendor's release cadence and portal structure better than someone who works with it professionally. That connection needs to be disclosed, not hidden:

- **Disclose the relationship** in the pull request or issue when contributing to, or reviewing, anything concerning your employer's (or client's) products — a source registration, a collector, a version correction, or a security advisory mapping.
- **A disclosed conflict does not disqualify a contribution.** It does mean that a *contested* change touching that vendor should get a second reviewer without the same conflict before merge, and that the contributor should not be the sole approver of their own PR in that area even if they hold collector-maintainer status for it.
- **This cuts both ways.** A correction that makes a vendor look worse (a withdrawn release, a security advisory mapping) is held to the same evidence standard as one that makes them look better. Neither favourable nor unfavourable framing is a reason to relax the evidence bar in [`docs/collectors/dataset-correction-policy.md`](docs/collectors/dataset-correction-policy.md).
