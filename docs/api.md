---
title: API
tags: [api, reference]
date: 2026-11-19
---

Beacon has one REST API, under `/api/v1`. The web interface will be built on it, and automation uses the same routes with an API token. This page describes how the API behaves as a whole, and the routes of alerts. The routes of other things are described on the pages about those things; [Routes](#routes) lists them.

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

A list that can grow without bound is **paged**. It takes `limit`, 50 unless given and 200 at most, and answers `next_cursor` when there is more: send it back as `cursor`, with the same filters, for the next page. A cursor is opaque, and a list is complete when its answer has no `next_cursor`. Paging does not skip or repeat entries when the list changes between pages. The alerts and their timelines are paged today. Lists of what the configuration defines are as long as the files and are answered whole.

**Filters** are query parameters. A filter that takes several values takes them repeated or separated by commas: `?status=firing&status=acknowledged` and `?status=firing,acknowledged` are the same. A parameter that a list does not know is refused with `400`.

**Times** are RFC 3339 in UTC, such as `2026-12-01T10:12:00Z`. A time in a request may be in any zone. **Durations** are written as in the configuration: `30m`, `2h`, `1d`, `1w`.

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

## Routes

| Routes | Described in |
|---|---|
| `/api/v1/alerts`, `/api/v1/alert-groups` | This page |
| `/api/v1/silences` | [[silences#API]] |
| `/api/v1/schedules`, `/api/v1/teams/<name>/on-call` | [[schedules#Who is on call]], [[schedules#Overrides]] |
| `/api/v1/me`, `/api/v1/me/identities` | [[access]], [[access#Chat accounts]] |
| `/api/v1/tokens` | [[access#API tokens]] |
| `/api/v1/config/reload` | [[configuration#Reload without a restart]] |
| `/api/v1/ingest/<source>` | [[webhook]], [[alertmanager]], [[grafana]]: how alerts come in, with a source's own token |
| `/api/v1/integrations/<name>/interactions` | [[slack#Request URL]], [[discord#Interactions endpoint]]: what chat services send |
