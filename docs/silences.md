---
title: Silences
tags: [processing, reference]
date: 2026-11-18
---

A silence keeps Beacon quiet about the alerts that match some labels, for a period: during planned work, or while a known problem is being fixed. Silences are runtime state. People create and end them through the API; they are not part of the configuration files.

A silenced alert is still received, stored and shown with its timeline. It is only kept out of notifications.

## What a silence is

| Part | Meaning |
|---|---|
| Matchers | One to twenty conditions on labels. An alert is silenced when **all** of them match. |
| Start | When the silence begins. Now, unless a later time is given. |
| End | When it ends. An alert is no longer silenced from this moment on. |
| Author | Who created it: the signed-in user, or the API token. Beacon records it; it cannot be chosen. |
| Comment | Why. Required. |

A matcher is a label, an operator and a value:

| Operator | Matches an alert whose label |
|---|---|
| `=` | has exactly the value |
| `!=` | has any other value |
| `=~` | matches the regular expression, as a whole |
| `!~` | does not match the regular expression |

- A label that an alert does not have counts as empty.
- Regular expressions use [RE2 syntax](https://github.com/google/re2/wiki/Syntax) and are anchored at both ends, as in Prometheus and in routing rules: `db[0-9]+` matches `db1` and not `db1-replica`. RE2 matches in time linear in the length of the value whatever the pattern.
- At least one matcher must not match an empty value. Otherwise a silence such as `team != "dba"` would cover every alert that has no `team` label at all.

A silence is `pending` before its start, `active` during its period, and `expired` afterwards.

## What a silence does

Beacon looks at silences when it is about to notify about an alert group ([[notifications#Groups]]), not when an alert arrives. An alert is silenced at that moment if a silence in force matches its labels. So a silence created now covers alerts that are already open, and a silence stops covering an alert the moment it ends.

| Situation | What happens |
|---|---|
| An alert arrives under a silence | It is stored, `firing`, in its group. No notification is sent for it. When that leaves the group with nothing to tell, the group's timeline gets a `notification_silenced` event with the silence and its end. |
| Some alerts of a group are silenced and some are not | The group is told about with the alerts that are not. The silenced ones are left out of the message. |
| A silenced alert resolves | Nothing is sent. Nobody was told that it fired, so nobody is told that it stopped. |
| The silence ends, or is expired, and the alert still fires | The alert is news now: its group is told about it, as a first notification or as an update. An expired silence takes effect at once, not at the end it used to have. |
| An alert that people were already told about is silenced | Its message stays as it is and nothing more is sent because of it. If it resolves, the message is updated to say so, so that it does not show a problem that is over. |
| Someone presses a button on a message | The message is updated, silence or not: that is an answer to a person, not a notification. |

A silence does not change the state of an alert, and it does not stop anyone from acknowledging or resolving it. Only a silence in force counts: a `pending` or `expired` one does nothing.

> [!note] Silences in Alertmanager and Grafana
> Those apply before Beacon hears of an alert: an alert silenced there is never sent, and Beacon does not know it exists. A silence in Beacon keeps the alert on record.

## Who may do what

Every route needs a signed-in user or an API token; see [[access]]. **Members and admins alike may list, create and expire silences**, including silences that someone else created: silencing is part of responding to alerts, and whoever is on call must be able to end a silence that was left behind. The author and the time are stored with each silence, and creating and expiring are logged with who did it.

## API

```
GET    /api/v1/silences            the active and pending silences
GET    /api/v1/silences?expired=true   those and the expired ones
POST   /api/v1/silences            create one
GET    /api/v1/silences/<id>       one silence
DELETE /api/v1/silences/<id>       expire one
```

### Create

```sh
curl -X POST https://beacon.example.com/api/v1/silences \
  -H "Authorization: Bearer $BEACON_TOKEN" \
  -d '{
    "matchers": [
      {"label": "alertname", "operator": "=", "value": "DiskFull"},
      {"label": "host", "operator": "=~", "value": "db[0-9]+"}
    ],
    "duration": "2h",
    "comment": "replacing the disks"
  }'
```

| Field | Required | Meaning |
|---|---|---|
| `matchers` | yes | One to twenty of `{"label", "operator", "value"}`. A label name is at most 200 characters, a value or pattern at most 1024. |
| `starts_at` | no | When the silence begins, in RFC 3339 form. Defaults to now; a time in the past is taken as now. At most a year ahead. |
| `ends_at` | one of | When it ends, in RFC 3339 form. In the future, after the start, and at most a year after it. |
| `duration` | one of | How long it lasts from its start, such as `30m`, `2h` or `1d`, in place of `ends_at` |
| `comment` | yes | Why, at most 1000 characters |

The body may be at most 64 KiB, and a field that is not in this table is an error. The answer is `201` with the silence:

```json
{
  "id": "5b0c1f0e-3c0a-4a53-9a39-0f8f3a2d6c11",
  "matchers": [
    {"label": "alertname", "operator": "=", "value": "DiskFull"},
    {"label": "host", "operator": "=~", "value": "db[0-9]+"}
  ],
  "starts_at": "2026-03-01T12:00:00Z",
  "ends_at": "2026-03-01T14:00:00Z",
  "created_by": "ada@example.com",
  "comment": "replacing the disks",
  "created_at": "2026-03-01T12:00:00Z",
  "state": "active"
}
```

`created_by` is the email address of the user who created the silence. A silence created with an API token has `created_by_token`, the name of the token, in its place. Times are answered in UTC.

### List and read

`GET /api/v1/silences` answers `{"silences": [...]}` with the active and pending silences, the one that ends first at the top. With `?expired=true` the expired ones follow, the most recently ended first. At most 1000 are returned. `GET /api/v1/silences/<id>` answers one silence, whatever its state.

### Expire

`DELETE /api/v1/silences/<id>` ends a silence now and answers `204`. The silence is kept, with its end set to the time it was expired; a silence that had not begun will never begin. Expiring a silence that is over already changes nothing and also answers `204`. A silence cannot be edited: expire it and create another.

### Errors

| Status | Meaning |
|---|---|
| `400` | The request is not a silence. `error` says why and `field` names the part at fault, such as `matchers[1].value`. |
| `401` | No valid session or token |
| `403` | A request from another site in a browser |
| `404` | No silence has this identifier |
| `413` | The body is larger than 64 KiB |

```json
{"error": "is not a regular expression: missing closing ): `^(?:(x)$`", "field": "matchers[0].value"}
```
