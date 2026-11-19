---
title: API
tags: [api, reference]
date: 2026-11-19
---

Beacon has one REST API, under `/api/v1`. The web interface will be built on it, and automation uses the same routes with an API token. This page describes how the API behaves as a whole, and the routes of alerts. The routes of other things are described on the pages about those things; [Routes](#routes) lists them.

## OpenAPI description

The API is described in OpenAPI 3.0: every route, who may call it, its parameters and the shape of every answer.

- A running Beacon serves it at `GET /api/v1/openapi.json` and `GET /api/v1/openapi.yaml`, without a credential. It describes the version that is running.
- It is published with this documentation as [openapi.yaml](openapi.yaml), and [[api-reference]] lists every route from it.

The description is written by hand and held to the server by tests, so it does not drift: a route that is not described, or a described route that does not exist, fails the build, and so does an answer of the real server that has a field the description lacks, or lacks one it requires. Each operation carries `x-beacon-access`, which is `public`, `member` or `admin`.

## Who may call it

Every route needs a credential, except the few that [[access#Who may use what]] lists as open. There are two:

- **A session**, which a person gets by signing in ([[access]]). A browser sends it as a cookie.
- **An API token**, sent as `Authorization: Bearer bcn_…` ([[access#API tokens]]). A request with that header is judged by the token alone.

Each has a role, `member` or `admin`. Members read everything and act on runtime state: alerts, silences, overrides. Admins also manage tokens and reload the configuration.

| Status | Meaning |
|---|---|
| `401` | No valid credential. An address that does not exist answers `401` too, so that a stranger cannot map the API. |
| `403` | The credential lacks the role, or a browser sent a request that changes something from another site |

## Conventions

These hold for every route.

**JSON in and out.** A request body is JSON, and unknown fields in it are refused, so that a typing mistake is not silently ignored. Every answer is JSON with `Cache-Control: no-store`, except `204`, which has no body.

**Errors** are an object with `error`, a sentence for a person, and `field` when one part of the request is at fault:

```json
{"error": "must be firing, acknowledged, snoozed, resolved or open", "field": "status"}
```

| Status | Meaning |
|---|---|
| `400` | The request is not one Beacon can carry out as written. `field` names the query parameter or the field of the body. |
| `404` | There is no such thing. An identifier of the wrong form answers `404` like one that nothing has. |
| `409` | The request was understood and the state does not allow it, such as acknowledging an alert that is resolved |
| `413` | The body is larger than the route accepts |
| `422` | Only from the reload route: the configuration files have problems |
| `500` | Beacon failed. The answer says `internal error`; the cause is in Beacon's log, under the request's identifier. |

Every answer carries `X-Request-Id`, which is also on each log record of the request.

**Lists** are an object with one array, named after what it holds:

```json
{"alerts": [{"id": "…"}, {"id": "…"}], "next_cursor": "eyJ0IjoiMjAyNi0xMi0wMVQxMDoxMjowMFoiLCJpZCI6IuKApiJ9"}
```

A list that can grow without bound is **paged**. It takes `limit`, 50 unless given and 200 at most, and answers `next_cursor` when there is more: send it back as `cursor`, with the same filters, for the next page. A cursor is opaque, and a list is complete when its answer has no `next_cursor`. Alerts, their timelines and users continue from a position in their order, so a page never skips or repeats an entry when the list changes in between. Silences, overrides and tokens are short lists that Beacon reads whole (a thousand entries at most) and pages by position, so an entry can shift between two pages when the list changes. Lists of what the configuration defines are as long as the files and are answered whole, without `next_cursor`.

**Filters** are query parameters. A filter that takes several values takes them repeated or separated by commas: `?status=firing&status=acknowledged` and `?status=firing,acknowledged` are the same. A parameter that a list does not know is refused with `400`.

**Times** are RFC 3339 in UTC, such as `2026-12-01T10:12:00Z`, sometimes with fractions of a second. A time in a request may be in any zone. **Durations** are written as in the configuration: `30m`, `2h`, `1d`, `1w`.

**Absent values** are `null`, or left out where a page says so. They are never empty strings. Arrays and objects are never `null`.

**Names and identifiers.** What the configuration defines is addressed by its `name`: a schedule, a team, an integration. Runtime state has an `id`, a UUID: an alert, a group, a silence, an override, a token.

**Provisioned objects are read-only.** Sources, integrations, teams, schedules, escalation policies and routing rules are defined in the configuration files ([[configuration]]); the API shows them and never changes them.

## Alerts

All routes of alerts are open to members: seeing alerts and responding to them is what being on call is.

| Route | What it does |
|---|---|
| `GET /api/v1/alerts` | Lists alerts, newest first |
| `GET /api/v1/alerts/<id>` | One alert |
| `GET /api/v1/alerts/<id>/timeline` | What happened to the alert, oldest first |
| `POST /api/v1/alerts/<id>/acknowledge` | Acknowledges it |
| `POST /api/v1/alerts/<id>/snooze` | Snoozes it |
| `POST /api/v1/alerts/<id>/resolve` | Resolves it by hand |
| `GET /api/v1/alert-groups/<id>` | A group with its alerts |
| `POST /api/v1/alert-groups/<id>/acknowledge`, `…/snooze`, `…/resolve` | The same actions, on every alert of the group that allows them |

### An alert

```json
{
  "id": "9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90",
  "group_id": "3f0e9a0c-6f6e-4a53-9a5e-0d6a4b0c2a11",
  "source": "prod",
  "fingerprint": "8d1f0c5b9e2a4f67",
  "status": "firing",
  "severity": "critical",
  "labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"},
  "annotations": {"summary": "Disk is full on db1", "runbook_url": "https://runbooks.example.com/disk"},
  "links": {"generator": "https://prometheus.example.com/graph?g0.expr=up"},
  "team": "platform",
  "service": "postgres",
  "routing_rule": "platform-critical",
  "started_at": "2026-12-01T10:12:00Z",
  "last_received_at": "2026-12-01T10:17:00Z",
  "acknowledged_at": null,
  "snoozed_until": null,
  "resolved_at": null,
  "actions": ["acknowledge", "snooze", "resolve"]
}
```

| Field | Meaning |
|---|---|
| `status` | `firing`, `acknowledged`, `snoozed` or `resolved` ([[alert-lifecycle]]) |
| `severity` | `critical`, `warning` or `info` |
| `source` | The name of the source that sent it |
| `fingerprint` | What identifies the alert at its source |
| `links` | The addresses the source sent with the alert, by name: `generator`, `dashboard`, `panel`, `silence` |
| `team`, `service`, `routing_rule` | What routing assigned when the alert opened ([[configuration#How an alert is routed]]), each `null` when nothing was |
| `last_received_at` | When the source last reported the alert as firing |
| `actions` | What can be done to the alert in its state. An action that is not listed answers `409`. |

### List

`GET /api/v1/alerts` takes these filters, all optional:

| Parameter | Values |
|---|---|
| `status` | `firing`, `acknowledged`, `snoozed`, `resolved`, or `open` for everything that is not resolved. Several may be given. |
| `severity` | `critical`, `warning`, `info`. Several may be given. |
| `team` | The name of a team |
| `service` | The name of a service |
| `source` | The name of a source |
| `group` | The identifier of an alert group |
| `limit`, `cursor` | Paging, as above |

```sh
curl -H "Authorization: Bearer $BEACON_TOKEN" \
  'https://beacon.example.com/api/v1/alerts?status=open&severity=critical&team=platform'
```

Alerts are ordered by when they began to fire, newest first. To keep a view up to date, ask again: an alert that arrived since is at the top of the first page.

### Timeline

`GET /api/v1/alerts/<id>/timeline` answers the events of the alert and of its group, oldest first, paged:

```json
{"events": [
  {"id": 4101, "alert_id": "9b2d6c1e-…", "group_id": "3f0e9a0c-…", "type": "fired", "occurred_at": "2026-12-01T10:12:00Z",
   "actor": {"type": "source", "id": "5c1e…", "name": null, "provider": null, "external_id": null}, "data": {}},
  {"id": 4102, "alert_id": null, "group_id": "3f0e9a0c-…", "type": "escalated", "occurred_at": "2026-12-01T10:12:30Z",
   "actor": {"type": "system", "id": null, "name": null, "provider": null, "external_id": null},
   "data": {"policy": "platform-default", "step": 1, "steps": 2, "pass": 1, "passes": 1, "timeout": "5m", "notified": [], "skipped": []}},
  {"id": 4107, "alert_id": "9b2d6c1e-…", "group_id": "3f0e9a0c-…", "type": "acknowledged", "occurred_at": "2026-12-01T10:14:10Z",
   "actor": {"type": "user", "id": "7a0d…", "name": "Ada Lovelace", "provider": "slack", "external_id": "U0123ABCDEF"}, "data": {"from": "firing", "to": "acknowledged"}}
]}
```

- `alert_id` is `null` for an event about the whole group: a notification, an escalation step.
- `actor.type` is `user`, `api_token`, `source` or `system`. `provider` and `external_id` name the chat account a person acted from, when they did.
- `data` depends on `type`. The types and what they hold are described where they are written: [[alert-lifecycle#Timeline]], [[notifications#Tracking]] and [[escalation#On the timeline]]. A client should show a type it does not know by its name.

### Act on an alert

`acknowledge` and `resolve` take no body. `snooze` takes when the snooze ends, as a time or as a duration from now, 30 days at most:

```sh
curl -X POST -H "Authorization: Bearer $BEACON_TOKEN" \
  https://beacon.example.com/api/v1/alerts/9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90/snooze \
  -d '{"duration": "1h"}'        # or {"until": "2026-12-01T12:00:00Z"}
```

Each answers `200` with the alert as it is afterwards, and `409` when the alert's state does not allow the action, such as acknowledging an alert that is already acknowledged. Whoever acts is recorded on the timeline: the signed-in user, or the API token by its name. The messages about the alert in chat change as they do after an action from chat.

Through the API, every member may acknowledge and snooze: a session and a token both say who is acting, which is what those two actions need ([[slack#Who can act]]).

### Groups

Alerts are told about in groups ([[configuration#Grouping]]), and an action from chat acts on the whole group. The API does the same under `/api/v1/alert-groups/<id>`, where `<id>` is an alert's `group_id`:

```json
{
  "id": "3f0e9a0c-6f6e-4a53-9a5e-0d6a4b0c2a11",
  "status": "open",
  "labels": {"alertname": "DiskFull"},
  "team": "platform",
  "routing_rule": "platform-critical",
  "escalation_policy": "platform-default",
  "created_at": "2026-12-01T10:12:00Z",
  "notified_at": "2026-12-01T10:12:30Z",
  "resolved_at": null,
  "alerts": [],
  "actions": ["acknowledge", "snooze", "resolve"]
}
```

`status` is `open`, or `resolved` once every alert of the group is. `alerts` holds the alerts of the group, oldest first, each as above. An action on a group is taken by every alert whose state allows it and answers the group as it is afterwards; it answers `409` only when no alert could take it.

## What the configuration defines

Sources, integrations, teams, schedules, escalation policies and routing are defined in the files, and the API shows the configuration that is active, to members, for reading. Each object carries `"provisioned": true`, which says that it is managed in the configuration and cannot be changed here. A list is in the order of the files; one object is read by its name, and a name that the configuration does not have answers `404`.

| Route | Answers |
|---|---|
| `GET /api/v1/sources`, `…/<name>` | `{"sources": [...]}`: `name`, `type`, `ingest_path`, the `labels` it adds, and how it derives `severity`, with the defaults filled in |
| `GET /api/v1/integrations`, `…/<name>` | `{"integrations": [...]}`: `name`, `type`, `interactions_path` for Slack and Discord, and `settings` |
| `GET /api/v1/escalation-policies`, `…/<name>` | `{"escalation_policies": [...]}`: `name`, `repeat`, and `steps`, each with its `timeout` and whom it notifies |
| `GET /api/v1/routing` | The routing block: the grouping, the `default` route, and the `rules` in the order they are tried. `404` when the configuration has none. |
| `GET /api/v1/teams`, `…/<name>` | `{"teams": [...]}`: what the files say and who belongs now |
| `GET /api/v1/schedules`, `…/<name>` | Schedules, with who is on call: [[schedules#Who is on call]] |

**No credential is answered, and no reference to one.** The `settings` of an integration hold what is not secret:

| Type | `settings` |
|---|---|
| `slack` | Nothing |
| `discord` | `application_id`, `public_key` |
| `email` | `host`, `port`, `tls`, `from`, and `authenticated`: whether Beacon signs in |
| `webhook` | `url`, without its query and its user information, where a token may be, and `headers`: the names of the configured headers, without their values |

A step's target says its `kind`, which is `schedule`, `team`, `user`, `channel` or `webhook`, and names what it is; the other names are `null`:

```json
{"name": "platform-default", "repeat": 1, "provisioned": true, "steps": [
  {"timeout": "5m", "notify": [
    {"kind": "schedule", "schedule": "platform-primary", "team": null, "user": null, "channel": null, "integration": null, "via": []},
    {"kind": "channel", "schedule": null, "team": null, "user": null, "channel": "C0123456789", "integration": "slack", "via": []}]}]}
```

A routing rule is answered with the grouping that applies to its alerts: its own, or the routing block's where it sets none.

### Teams and users

```json
{"name": "platform", "display_name": "Platform", "groups": ["platform-engineers"], "provisioned": true,
 "members": [{"email": "ada@example.com", "display_name": "Ada Lovelace", "origins": ["config", "group"]}],
 "schedules": ["platform-primary"],
 "skipped_members": ["grace@example.com"]}
```

- `members` are the users who belong to the team now. `origins` says why: `config` for the `members` list of the files, `group` for an identity provider group ([[configuration#Team membership]]).
- **`skipped_members` is answered to admins only**: the addresses in the files that no user has, because the person has not signed in yet. For a member the field is left out.
- `schedules` are the names of the schedules the team owns.

`GET /api/v1/users` lists the users, by email address, paged, with the filters `team` and `role`:

```json
{"users": [{"id": "7a0d…", "email": "ada@example.com", "display_name": "Ada Lovelace", "role": "admin", "disabled": false,
            "created_at": "2026-11-02T09:00:00Z", "last_sign_in_at": "2026-12-01T08:30:00Z", "teams": ["platform"]}]}
```

Users are created by signing in and cannot be changed through the API.

`GET /auth/methods` needs no credential and says which ways to sign in exist, for a sign-in page: `{"oidc": true, "bootstrap_admin": false}`.

## Routes

| Routes | Described in |
|---|---|
| `/api/v1/alerts`, `/api/v1/alert-groups` | This page |
| `/api/v1/sources`, `/api/v1/integrations`, `/api/v1/escalation-policies`, `/api/v1/routing`, `/api/v1/teams`, `/api/v1/users` | This page |
| `/api/v1/silences` | [[silences#API]] |
| `/api/v1/schedules`, `/api/v1/teams/<name>/on-call` | [[schedules#Who is on call]], [[schedules#Overrides]] |
| `/api/v1/me`, `/api/v1/me/identities` | [[access]], [[access#Chat accounts]] |
| `/api/v1/tokens` | [[access#API tokens]] |
| `/api/v1/config/reload` | [[configuration#Reload without a restart]] |
| Every route, in one table | [[api-reference]] |
| `/api/v1/ingest/<source>` | [[webhook]], [[alertmanager]], [[grafana]]: how alerts come in, with a source's own token |
| `/api/v1/integrations/<name>/interactions` | [[slack#Request URL]], [[discord#Interactions endpoint]]: what chat services send |
