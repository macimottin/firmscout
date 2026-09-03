# Security Policy

## Supported versions

FirmScout has not published a release yet. The project is pre-alpha, with the first vertical slice still under construction, and there is no production deployment. There is currently no "supported version" in the usual sense — there is only the `main` branch.

Once versioned releases exist, this section will state which lines receive security fixes. Until then, assume only `main` is in scope, and that anything reported may already be moot by the time a release model exists.

## Reporting a vulnerability

**Please do not open a public issue for a suspected security vulnerability.**

Preferred: use [GitHub's private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability) on this repository (Security tab → "Report a vulnerability"). This opens a private advisory visible only to maintainers until a fix is ready.

Alternative: email **TODO: set the security contact address**.

Please include:

- What you found, and where (file, endpoint, source registration flow, etc.)
- Steps to reproduce, or a proof of concept
- What you think the impact is
- Whether you're aware of it being exploited

## Response targets

These are **targets we aim for, not commitments we're contractually bound to** — the project has no paid support tier yet, and response time depends entirely on maintainer availability.

| Stage | Target |
| --- | --- |
| Acknowledgement of report | 3 business days |
| Initial assessment (severity, whether it's in scope) | 7 business days |
| Fix or mitigation for a confirmed high/critical issue | Best effort, prioritized over other work |

## Scope

FirmScout's architecture creates specific risk classes that matter more here than in a typical web application, because the system's whole job is to fetch content from URLs it does not control and to let non-programmers register new sources via pull request. The classes we most want to hear about:

- **SSRF via source registration** — a registered source URL, or a redirect from one, reaching an internal network address, cloud metadata endpoint, or otherwise unintended target through the fetcher.
- **Collector sandbox escape** — a collector (config-driven or code-based) doing anything beyond fetching a URL and extracting structured data from the response: filesystem access, arbitrary outbound requests, resource exhaustion beyond configured limits.
- **Secret leakage in logs or artifacts** — API keys, database credentials, or AI provider credentials appearing in logs, stored artifacts, traces, or error responses.
- **API key handling** — issues in how keys are generated, hashed, displayed, revoked, or scoped.
- **Abuse of the public API** — anything that defeats rate limiting, quota enforcement, or entitlement checks in a way that isn't just "expected free-tier friction."
- **Supply-chain risk in collector configurations** — a collector config (YAML) or a code-based collector contribution designed to exfiltrate data, perform SSRF, or otherwise act maliciously once merged and run.

## Out of scope

- **Findings against manufacturer websites.** A vulnerability in MikroTik's, Fortinet's, Poly's, Dell's, or any other vendor's own infrastructure is not FirmScout's to receive or triage — report it to that vendor directly.
- **Reports produced by bypassing vendor access controls.** If reproducing your finding required defeating a CAPTCHA, working around authentication, or otherwise violating a vendor's terms of service or `robots.txt`, it's out of scope here for the same reason it's out of scope for collection — see [DATA_SOURCES.md](DATA_SOURCES.md) and [CONTRIBUTING.md](CONTRIBUTING.md#source-compliance-rules--non-negotiable).
- Denial-of-service reports that require volumes far beyond anything a realistic attacker would deploy against a project at this stage, without a novel amplification factor.
- Reports with no actionable detail (a scanner output dump with no analysis is not a report).

## Disclosure

We ask for coordinated disclosure: please give us a reasonable window to investigate and, where warranted, ship a fix before any public discussion. We'll credit reporters (with permission) once an issue is resolved.
