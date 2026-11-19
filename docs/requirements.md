---
title: Requirements
tags: [planning, requirements]
date: 2026-11-16
---

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

Each requirement is marked **MVP** (needed for the first usable release) or **Later** (a planned enhancement, not a commitment to order).

## Principles

- **Five minutes to the first page.** A new user gets from `docker compose up` to a test page in Slack or Discord in about five minutes.
- **Configuration as code first.** Sources, integrations, teams, routing and escalation policies are defined in YAML files that live in version control. The database holds users and runtime state.
- **One binary plus Postgres.** Beacon ships as a single Go binary and needs only a Postgres database, so it can scale out without a storage migration.
- **Works with what people already run.** Beacon sits behind an existing Alertmanager or Grafana setup; adopting it does not require changing Prometheus.
- **Observable by default.** Beacon exposes its own state as Prometheus metrics, so the same stack that produces alerts can report on how they were handled.

## Ingestion

| Requirement | Phase |
|---|---|
| Alertmanager webhook receiver | MVP |
| Grafana alerting webhook receiver | MVP |
| Generic JSON webhook | MVP |
| Direct Prometheus endpoint (Alertmanager API compatible) | Later |
| Email ingestion | Later |
| Datadog, CloudWatch and Sentry integrations | Later |
| Heartbeat (dead man's switch) checks | Later |

## Processing

| Requirement | Phase |
|---|---|
| Deduplication by fingerprint | MVP |
| Grouping of related alerts | MVP |
| Label-based routing to a team or service | MVP |
| Severity levels | MVP |
| Manual silences | MVP |
| Inhibition | Later |
| Scheduled maintenance windows | Later |
| Flap detection | Later |
| Correlation of related alerts | Later |

## On-call schedules

| Requirement | Phase |
|---|---|
| Rotations (daily, weekly, custom length) | MVP |
| Overrides | MVP |
| "Who is on call now" view | MVP |
| Past shifts are recorded and never recomputed when a schedule definition changes | MVP |
| A changed definition takes effect at the next handoff, or at a stated date, and continues from whoever is on call | MVP |
| Preview command that shows which upcoming shifts a definition change would move | MVP |
| Layered and follow-the-sun schedules | Later |
| Shift swaps | Later |
| Subscribable calendar feed per schedule and per person, showing shifts and reflecting changes | Later |
| Native Google Calendar sync, for prompt updates when shifts change | Later |
| Sync the current on-call to platform groups (Slack user groups first, then others) | Later |

> [!note] Stable schedules under code management
> The file states the intent: members, order, shift length and handoff time. The database holds the position: who is on call now and when their shift began. Editing the file therefore never reshuffles shifts that have happened or the shift in progress, and the preview command shows the effect on upcoming shifts before the change is merged.

> [!note] Calendar feeds
> A subscribable iCal feed works in Google Calendar, Outlook and Apple Calendar from one implementation. Google refreshes subscribed feeds slowly, so overrides and swaps can appear late. Native Google Calendar sync is listed separately for that reason.

## Escalation

| Requirement | Phase |
|---|---|
| Multi-step policies with timeouts | MVP |
| Repeat until acknowledged | MVP |
| One page per alert group: a group that has been acknowledged never pages again because of new alerts | MVP |
| Opt-in re-page policy: page again when a group is still unresolved after a stated time, also after it was acknowledged ([#100](https://github.com/thanoslabs/beacon/issues/100)) | Later |
| Round-robin steps | Later |
| Conditional steps by severity or time of day | Later |

## Notifications

| Requirement | Phase |
|---|---|
| Slack | MVP |
| Discord | MVP |
| Email | MVP |
| Outbound webhook | MVP |
| Retries and delivery tracking | MVP |
| SMS through Twilio | Later |
| Voice calls | Later |
| Teams and Telegram | Later |
| Mobile app with push notifications | Later |
| Recurring summary of alert activity, so alerts that stay firing are not forgotten ([#101](https://github.com/thanoslabs/beacon/issues/101)) | Later |
| Rich per-user notification rules | Later |

## Alert lifecycle

| Requirement | Phase |
|---|---|
| Fire, acknowledge, resolve and snooze | MVP |
| Auto-resolve when the source resolves | MVP |
| Acknowledge and resolve from Slack and Discord | MVP |
| Per-alert timeline of events and notifications | MVP |
| Notes and comments | Later |
| Incidents with roles and postmortems | Later |
| Status pages | Later |

## Configuration

Beacon separates what is provisioned from what happens at runtime. Provisioned objects are defined as code and are read-only in the UI. Runtime state lives in Postgres and is changed through the UI, the API and chat.

| Kind | Managed as | Examples |
|---|---|---|
| Provisioned | Code | Sources, notification integrations, routing rules, escalation policies, teams, schedule rotations |
| Runtime | Database | Users, alert state, acknowledgements, snoozes, silences, schedule overrides, notification history |

| Requirement | Phase |
|---|---|
| Declarative YAML configuration files for all provisioned objects | MVP |
| Validation command that checks configuration without applying it, for use in CI | MVP |
| Reload configuration without restart | MVP |
| Clear errors that name the file, object and field at fault | MVP |
| Terraform provider | Later |
| Kubernetes CRDs | Later |
| Optional UI editing of provisioned objects, for installs that do not use files | Later |

## Interface

| Requirement | Phase |
|---|---|
| Web UI, built as a separate single-page app on the REST API, to view alerts, schedules and escalation policies, and to act on runtime state (acknowledge, snooze, silence, override) | MVP |
| REST API for runtime state and read access to provisioned objects | MVP |
| API tokens | MVP |
| CLI | Later |

## Access

| Requirement | Phase |
|---|---|
| Sign-in through OAuth 2.0 and OpenID Connect | MVP |
| Users stored in the database, created on first sign-in | MVP |
| Admin and member roles, mapped from identity provider roles or groups | MVP |
| Team membership mapped from identity provider groups | MVP |
| Teams defined in configuration with a list of users; a listed user who does not exist is skipped and logged as an error | MVP |
| Link each user to their Slack and Discord identity | MVP |
| Bootstrap admin account for installs with no identity provider | MVP |
| SCIM provisioning, so users and groups sync without a sign-in | Later |
| Local users with passwords, managed in the UI | Later |
| Fine-grained RBAC | Later |
| Audit log | Later |
| Multi-tenancy | Later |

## Deployment

| Requirement | Phase |
|---|---|
| Single Go binary | MVP |
| Docker image | MVP |
| Postgres as the only required dependency | MVP |
| Docker Compose file that starts Beacon and Postgres together | MVP |
| Stateless server process, so more replicas can be added later | MVP |
| Frontend assets embedded in the binary for releases, so an install is still one binary | MVP |
| Helm chart | Later |
| High availability with multiple replicas | Later |
| Self-monitoring dead man's switch | Later |

## Metrics and insight

Beacon exposes a Prometheus `/metrics` endpoint. It covers both how Beacon is running and how alerts are being handled, so teams can build dashboards and alerts in the tools they already use before Beacon has any built-in reporting.

| Requirement | Phase |
|---|---|
| Operational metrics: ingestion rate, notification successes and failures by channel, queue depth, API latency | MVP |
| Alert-handling metrics: alerts by team, service and severity; time to acknowledge and time to resolve as histograms; escalations | MVP |
| On-call metrics: current on-call per schedule; pages per person | Later |
| Example Grafana dashboard and alert rules shipped with the project | Later |
| Built-in reports: noisiest alerts, on-call load, trends | Later |

> [!warning] Label cardinality
> Metric labels are limited to bounded sets such as team, service, severity and channel. Alert names and alert identifiers are not used as labels.

## Migration

| Requirement | Phase |
|---|---|
| Import from Alertmanager configuration | Later |
| Import from Opsgenie, PagerDuty and Grafana OnCall | Later |

## Decisions

| Decision | Choice | Reason |
|---|---|---|
| Relationship to Alertmanager | Sit behind it for the MVP; replace it later | Adoption needs no change to an existing Prometheus setup |
| Source of truth for configuration | Code for provisioned objects (sources, integrations, routing, escalation policies); the database for runtime state | Changes are reviewed and versioned, and alert state stays queryable |
| Notification channels in the MVP | Slack and Discord first, with email and outbound webhook; SMS and voice later | Chat is where teams already respond, and it avoids a paid provider at the start |
| Storage | Postgres from the start | Scalability is accounted for from the beginning, with no later migration |
| Incident management | Out of scope for now | It is a separate product; the gap is in alerting and paging |
| Users and teams | Users live in the database; teams are defined in configuration as lists of users, and unknown users are skipped and logged | People come and go through the identity provider, while team structure is reviewed like other configuration |
| Authentication | OAuth 2.0 and OpenID Connect first, with roles and team membership mapped from the provider's roles and groups | Access follows the organisation's existing identity provider |
| Configuration format | YAML | It is what the Prometheus audience already uses |
| Frontend | A separate single-page app that uses the REST API | The API is exercised by a real client from the start |
| Paging again | One page per alert group. New alerts that join an acknowledged group, or one whose policy ran out, update its messages and page nobody. A snooze that runs out pages again. Paging again after a stated time is a later, opt-in policy (#100) | Whoever acknowledged a group has taken the problem; being paged for every further alert of it is how people learn to ignore pages |
| A schedule with nobody on call | A gap in coverage is allowed and is logged as a warning, not an error | Some teams do not want cover at all times, and a rotation whose members have not signed in yet is not a fault |
