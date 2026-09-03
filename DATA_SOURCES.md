# Data Sources and Compliance Policy

This document is the practical, contributor-facing version of the compliance policy established in [blueprint §3.6](docs/architecture/blueprint.md#36-dell-as-the-clean-api-pilot--challenged-on-compliance-grounds) and [ADR-0018](docs/adr/0018-source-compliance-policy.md). If you're registering a new source, read this before opening a pull request.

## The principle

**Compliance status is a first-class field on every source, evaluated before collection, not discovered after the fact.** A source that would require bypassing authentication, a CAPTCHA, a rate limit, or `robots.txt` is not collected from — full stop — regardless of how technically convenient the data would be. This is enforced structurally: a source with a disqualifying compliance status is registered as `enabled = false`, visible in the dataset, and documented, rather than quietly omitted or quietly scraped anyway.

## Source quality classes

Every source is labelled with a quality class, and **third-party sources are labelled as such and never silently outrank an official one** — if an official source and a community source disagree, the official source wins by default, and the disagreement itself is surfaced (see [blueprint §16, gate 10](docs/architecture/blueprint.md#16-update-and-publication-rules)) rather than resolved by picking whichever was collected first.

| Class | Definition | Example |
| --- | --- | --- |
| **Official manufacturer source** | A domain controlled by the vendor itself: their downloads page, changelog, PSIRT feed, or official API. | `mikrotik.com`, `fortiguard.com` |
| **Authorised support portal** | A vendor-sanctioned third-party portal operating with the vendor's knowledge (e.g. after an acquisition moves documentation to a new domain, or a vendor uses a partner platform for support content). | `support.hp.com` hosting former Poly documentation |
| **Vendor-maintained repository** | A code or artifact repository the vendor operates directly (e.g. a GitHub org under the vendor's control) but that isn't the "primary" marketing/support domain. | A vendor's official GitHub releases page |
| **Trusted community source** | Not vendor-controlled, but maintained by a known, accountable party with a track record of accuracy (e.g. a well-established hardware enthusiast project, a distro's package repository). | — |
| **Unknown third party** | Anything else: a mirror, an aggregator, or a source whose relationship to the vendor hasn't been established. | — |

The full criteria for assigning a class, and how class interacts with confidence scoring and auto-publication thresholds, are in [`docs/collectors/source-quality-policy.md`](docs/collectors/source-quality-policy.md).

## `robots_policy_status` and `terms_review_status`

Every source carries two independent status fields, both evaluated **before** the source is enabled:

- **`robots_policy_status`** — the result of actually fetching and reading the host's `robots.txt`: `allowed`, `disallowed`, or `not_checked`. This is a mechanical, verifiable fact, not a judgment call — if `robots.txt` disallows the path you want to collect, the status is `disallowed`, and the source stays disabled until either the path changes or the vendor grants explicit permission.
- **`terms_review_status`** — whether the source's terms of service have been reviewed for anything that would make automated, low-volume, polite collection of publicly available version metadata a problem: `pending`, `reviewed_ok`, or `reviewed_blocked`. Terms review is a human judgment call (often needing legal input for anything ambiguous), unlike the mechanical `robots_policy_status`.

A source needs `robots_policy_status = allowed` **and** `terms_review_status = reviewed_ok` before it is enabled for automated collection. Either field short of that keeps `enabled = false`, regardless of how good the data would be.

## The four pilot vendors — measured, 2026-09-03

These are the sources evaluated for the MVP's first vertical slice. The facts below were measured directly, not assumed.

| Vendor | Source | Format | robots_policy_status | terms_review_status | Status | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| **MikroTik** | `upgrade.mikrotik.com/routeros/NEWESTa7.stable` | Plain text (`version epoch`), ETag + Last-Modified | `allowed` | `pending` | Registered, cheapest change signal available | Near-zero-cost watcher: a conditional GET against a tiny endpoint. |
| **MikroTik** | `mikrotik.com/download/changelogs` | Server-rendered HTML (structured `.changelog-header` entries: version, channel badge, date) | `allowed` | `pending` | Registered | No ETag; `Cache-Control: private` — requires normalised section hashing rather than a conditional request to detect change. |
| **Poly / HP** | Release notes, formerly `docs.poly.com`, now redirecting to `support.hp.com/bundle/...` | PDF, behind a JS-rendered shell | `not_checked` | `pending` | Registered, pilot for "source relocated after acquisition" | Demonstrates that source URLs are not permanent even for the vendor's own official documentation — the registry tracks the relocation. |
| **Fortinet** | PSIRT advisories: `fortiguard.com/rss/ir.xml` → `filestore.fortinet.com/fortiguard/rss/ir.xml` | RSS 2.0 (XML) | `not_checked` | `pending` | Registered, advisory feed only | Firmware **downloads** require authentication and are out of scope for automated collection; the public PSIRT feed is the pilot for "difficult portal, but an official public feed exists for part of the data." |
| **Dell** | `downloads.dell.com/catalog/Catalog.xml.gz` | Gzipped XML, ETag + Last-Modified, ~1.4 MB | **`disallowed`** | `pending` | **Registered but `enabled = false`** | Technically the ideal source for the "clean, structured catalogue" profile — but `downloads.dell.com/robots.txt` returns `Disallow: /` for all user agents (only `/manuals` and `/topicspdf` are permitted). Collecting it anyway would violate this project's own stated policy, in public, on day one. It stays in the registry, visibly disabled, pending either an official API or written permission from Dell. |

**The recommended substitute for the "clean API" pilot profile is Ubiquiti**, not Dell: `fw-update.ubnt.com/api/firmware-latest` is an official JSON (HAL) endpoint returning `channel`, `platform`, `product`, timestamps, and checksums, with no `robots.txt` restriction (the host returns 404 for `robots.txt`, which is treated as "no restriction stated" rather than "allowed by omission" — `terms_review_status` still needs to move from `pending` to `reviewed_ok` before this source is enabled). Ubiquiti is registered with `robots_policy_status = allowed`, `terms_review_status = pending`.

## Why Dell matters beyond one vendor

Dell's disallowed status is the clearest illustration of why compliance is a first-class field rather than an afterthought: it is the single most attractive source, technically, among the four pilots — a well-formed, cacheable, ETag-bearing catalogue — and it is the one the project refuses to collect from until the policy conflict is resolved. If FirmScout is willing to bend this rule for its most convenient source, the rule doesn't mean anything for the next thousand sources.

## Removal-request process

If a vendor asks that their data be removed or that FirmScout stop collecting from a specific source:

1. **The request is honoured promptly.** Open an issue (or, if the requester prefers privacy, use the security contact in [SECURITY.md](SECURITY.md) — vendor relations issues are handled with the same care as security reports) describing what was requested.
2. **The affected source(s) are set to `enabled = false` immediately**, pending any further conversation. Automated collection stops before the conversation about scope even finishes.
3. **Published facts already derived from that source are not silently deleted** — per the immutability principle, they are marked withdrawn with the removal request recorded as the reason and evidence, exactly like any other withdrawal. This preserves the audit trail (what did we publish, and why did it stop being published) without pretending the historical record never existed. If the vendor's request specifically requires deletion rather than withdrawal-with-reason, that's handled case by case and may need legal input — flag it clearly as such.
4. **The source's `robots_policy_status` or `terms_review_status` is updated** to reflect the new understanding, so the same conflict doesn't recur silently later.

## Attribution policy

Every published fact carries evidence pointing back to where it came from: the source URL, the retrieval timestamp, and (where the source class supports it) an excerpt. FirmScout does not present vendor-published information as if it originated with FirmScout — the official source link is a first-class, always-present field in the API and on the public site, not a footnote. For community-sourced or trusted-third-party data, the source and its quality class are shown alongside the fact, so a reader can tell "MikroTik says this" apart from "a community source we trust says this," and weight it accordingly.
