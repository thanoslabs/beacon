---
title: On-call schedules
tags: [schedules, on-call]
date: 2026-11-18
---

A schedule is a rotation: a list of people who take turns being on call. The rotation is defined in the configuration files ([[configuration#Schedules]]). Where the rotation is, who is on call now and since when, is kept in the database, together with every shift that has happened. The tables are described in [[configuration-and-data-model]].

```yaml
schedules:
  - name: platform-primary
    team: platform
    time_zone: Europe/London
    rotation:
      members:
        - ada@example.com
        - grace@example.com
        - linus@example.com
      shift_length: 1w
      handoff:
        day: monday
        time: "09:00"
```

A step of an escalation policy that names a schedule pages whoever is on call for it at that moment; see [[escalation]].

## The file is the intent, the database is the position

The file says who takes turns, in which order, for how long and when they hand over. It has no start date and no "current person". Beacon stores those itself:

| Stored | Meaning |
|---|---|
| The person on call | A person, not a place in the list |
| When their shift began | |
| When the next handoff is due | The timer of the rotation |
| The rotation in force | The definition the position was worked out with |

At each handoff the next member in the list takes over, and after the last the first again.

## When a rotation starts

A schedule starts the moment Beacon first applies a configuration that has it, with the first member on call. Its **first handoff is the first one that is at least a full shift length away**, so the first shift is never shorter than a normal one: a weekly rotation with its handoff on Monday at 09:00 that starts on a Tuesday first hands over on the Monday after next.

## Handoffs and time zones

`handoff.time` is a time on the clock in the schedule's `time_zone`, and `handoff.day` is a weekday there. Shift lengths are whole days or weeks. Each handoff is worked out from the calendar date, never by adding hours to the one before, so a handoff at 09:00 stays at 09:00 when the clocks change:

- The shift during which clocks go forward is an hour shorter, and the one during which they go back an hour longer.
- **A handoff in the hour that is skipped** when clocks go forward happens as much later as the clocks moved. A handoff at 02:30 on such a day is at 03:30. The next day it is at 02:30 again.
- **A handoff in the hour that comes twice** when clocks go back happens the first time.

Beacon uses the time zone database that is built into the binary, so the answer does not depend on the machine it runs on. A release of Beacon brings the zone rules of its Go version.

## Members who have not signed in

Members are named by email address, and a person becomes a user in Beacon when they first sign in ([[access]]). A member whose address no user has is **passed over**: the rotation goes on to the next member who is a user.

- Each such address is logged as an error, `rotation member skipped`, with the schedule and the address, every time the configuration is applied.
- The metric `beacon_schedule_members_skipped{schedule="..."}` counts them per schedule.
- The person takes their place in the order at the first handoff after they have signed in. No reload is needed.
- When no member is a user, **nobody is on call**: the schedule has a gap in coverage. That is a state a team may want, so it is not an error. Beacon logs a warning, `gap in coverage: the schedule has nobody on call`, when the configuration is applied and at every handoff to nobody. An escalation step that names the schedule during the gap pages nobody for it, says so on the timeline, and goes on with its other targets ([[escalation#Who is skipped]]). The first member to sign in is on call from that moment until the handoff that was planned, and an override covers a gap at any time.

## Shifts are recorded as they happen

Every shift is a row: who, on which schedule, from when and until when. The row is written when the shift begins, without an end, and its end is set at the handoff. **A shift is never recomputed.** Nothing that happens later, such as a change to the definition, touches a shift that has been recorded, and the database itself refuses any change to a shift that has ended.

- **Late is not lost.** A handoff is carried out by a worker that looks for due handoffs every five seconds, and it is recorded at the moment it was due, not the moment it was carried out. If Beacon was not running when handoffs fell due, the next process to start records each shift that began in the meantime, with the times it began and ended at.
- **A restart in the middle of a shift changes nothing.** The position is in the database, not in the process.
- **Once, with several replicas.** The timer is a column in Postgres. A replica that carries out a handoff holds the schedule's position while it does, and the others skip it.

## Changing a schedule

Edit the schedule in the files and reload. **Applying a change never moves the shift in progress.** Beacon stores the new definition beside the position, as *pending*, and the rotation in force keeps running until the change takes over:

| The file | The change takes over |
|---|---|
| has no `effective_from` | at the next handoff of the rotation in force |
| has `effective_from` | at that time. Until then the rotation in force hands over as before. A time that has passed means now. |

**At a handoff** (the first row), the handoff happens when it would have, and the person who takes over is whoever follows the person on call *in the new order*. If the handoff time, the day or the time zone changed, that first shift under the new rotation ends at the first new handoff that leaves it a full shift, as for a new schedule.

**At a stated time** in the middle of a shift, whoever is on call stays on call. Their shift now ends at the first handoff of the new rotation after that time, which may be sooner or later than it would have been. A shift ends early in one case only: the new rotation no longer has the person on call. Whoever follows them takes the rest of that shift, from the stated time to the next handoff.

### Who is next

The position is the person on call, so everyone else keeps their place when the list changes:

- **Adding a member** puts them where the list puts them. Nobody else's turn changes order.
- **Removing a member** closes the gap. If they are on call, they finish their shift (unless a stated time relieves them), and the next person is the first member of the new list who came after them in the old order.
- **Reordering** takes effect from the person on call: the next person is whoever follows them in the new list.

Taking a change back before it takes over, by restoring the old definition, leaves nothing pending. Remove `effective_from` from the file once its time has passed: left in place, the next edit of the schedule would take over at once instead of at the next handoff.

### Preview a change

`beacon preview` shows what a configuration would do to upcoming shifts, without applying it:

```console
$ beacon preview --config beacon.d --weeks 4
schedule platform-primary: changed. The new definition takes over at the next handoff, Mon 2026-11-23 09:00 GMT.
  the person on call would change in 2 periods in the next 4 weeks:
    Mon 2026-11-23 09:00 GMT to Mon 2026-11-30 09:00 GMT: linus@example.com instead of grace@example.com
    Mon 2026-11-30 09:00 GMT to Mon 2026-12-07 09:00 GMT: grace@example.com instead of linus@example.com
  shifts with this configuration:
    Mon 2026-11-16 09:00 GMT to Mon 2026-11-23 09:00 GMT: ada@example.com
    Mon 2026-11-23 09:00 GMT to Mon 2026-11-30 09:00 GMT: linus@example.com
    ...
```

- It needs the configuration to preview (`--config`) and the database of the running Beacon (`BEACON_DATABASE_URL`), because the position is there. It only reads: the queries run in a read-only transaction, so a database user with `SELECT` on `schedules`, `schedule_positions` and `users` is enough. It needs no secret of the configuration.
- It runs the same arithmetic as the server, so what it shows is what will be recorded if nothing but time changes.
- For each schedule it says whether it is `new`, `unchanged`, `changed`, `pending` (applied already and waiting to take over) or `removed`, and lists the periods in which someone else would be on call. Times are shown in the schedule's zone.
- Members who are not users are listed as passed over, as they would be.
- `--format json` prints the same for a program to read. `--schedule <name>` limits the output to one schedule. `--weeks` sets how far ahead to look (default 8).
- **In CI**: `beacon preview` exits `0` whatever it finds, so its output can be posted to a pull request. With `--fail-on-moves` it exits `1` when any upcoming shift would move or a running schedule would end, for teams that want such a change to need a second look.

## Overrides

An override puts one person on call for a schedule for a period, in place of whoever the rotation has: someone covers a holiday, a doctor's appointment or a night. Overrides are runtime state. People create and remove them through the API; they are not part of the configuration files, and a reload does not touch them.

| Part | Meaning |
|---|---|
| Person | Any user, by email address. They need not be a member of the rotation. |
| Start | When the override begins. Now, unless a later time is given. |
| End | When it ends. From this moment the rotation's person is on call again. |
| Author | Who created it: the signed-in user, or the API token. Beacon records it; it cannot be chosen. |
| Reason | Why. Optional. |

- **The rotation goes on underneath.** An override does not move the rotation: handoffs happen when they would have, and when the override ends, the person on call is whoever the rotation has by then.
- **Overlapping overrides are refused.** A schedule has at most one override at any moment. A request for a period that overlaps an existing override is answered `409` and names the override in the way; remove that one, or choose a period beside it. Periods that touch, one ending as the next begins, do not overlap. Beacon does not pick a winner between two overrides, because whichever rule it used would surprise one of the two people.
- **Overrides are recorded as shifts.** When an override begins, a shift of kind `override` is written for its person, with the override's identifier, beside the rotation's shift, which stays as it is. It is closed when the override ends or is removed. Who was really on call at a past moment is the person of the override shift that covers it, and otherwise the person of the rotation shift.
- An override cannot be edited. Remove it and create another.
- When a schedule leaves the configuration, its overrides end with it.

### Who may do what

Every route needs a signed-in user or an API token; see [[access]]. **Members and admins alike may list, create and remove overrides**, for themselves and for other people, including overrides that someone else created: covering for a colleague is part of being on call. The author and the time are stored with each override, and creating and removing are logged with who did it.

### API

```
GET    /api/v1/schedules/<name>/overrides             the overrides in force and to come
GET    /api/v1/schedules/<name>/overrides?past=true   those, the ended and the removed ones
POST   /api/v1/schedules/<name>/overrides             create one
DELETE /api/v1/schedules/<name>/overrides/<id>        remove one
```

```sh
curl -X POST https://beacon.example.com/api/v1/schedules/platform-primary/overrides \
  -H "Authorization: Bearer $BEACON_TOKEN" \
  -d '{
    "user": "grace@example.com",
    "starts_at": "2026-12-24T09:00:00Z",
    "ends_at": "2026-12-27T09:00:00Z",
    "reason": "covering the holidays"
  }'
```

| Field | Required | Meaning |
|---|---|---|
| `user` | yes | Email address of the person who covers. They must be a user, which a person becomes at their first sign-in. |
| `starts_at` | no | When the override begins, in RFC 3339 form. Defaults to now; a time in the past is taken as now. At most a year ahead. |
| `ends_at` | one of | When it ends, in RFC 3339 form. In the future, after the start, and at most a year after it. |
| `duration` | one of | How long it lasts from its start, such as `8h`, `1d` or `1w`, in place of `ends_at` |
| `reason` | no | Why, at most 500 characters |

The body may be at most 16 KiB, and a field that is not in this table is an error. The answer is `201` with the override:

```json
{
  "id": "0d6f3b52-7a0e-4a0f-9d0b-0a6e5f1f3c77",
  "schedule": "platform-primary",
  "user": "grace@example.com",
  "starts_at": "2026-12-24T09:00:00Z",
  "ends_at": "2026-12-27T09:00:00Z",
  "reason": "covering the holidays",
  "created_by": "ada@example.com",
  "created_at": "2026-12-01T10:12:00Z",
  "state": "pending"
}
```

`state` is `pending` before the start, `active` during the period, `ended` afterwards and `cancelled` when the override was removed; a removed override also has `cancelled_at`. `created_by` is the email address of the user who created it; an override created with an API token has `created_by_token`, the name of the token, in its place. Times are answered in UTC.

A list answers `{"overrides": [...]}`, by start, at most 1000.

`DELETE` answers `204`. An override that has not begun never will; one that is in force ends now. Removing one that is over, or was removed before, changes nothing and also answers `204`.

| Status | Meaning |
|---|---|
| `400` | The request is not an override. `error` says why and `field` names the part at fault. |
| `401` | No valid session or token |
| `403` | A request from another site in a browser |
| `404` | No schedule has this name, or it has no override with this identifier |
| `409` | The period overlaps another override of the schedule |
| `413` | The body is larger than 16 KiB |

## Who is on call

The person on call for a schedule is the person of the override in force, and otherwise the person the rotation has. For a team it is the person on call for each schedule that the team owns.

The answer is worked out when it is asked for: a handoff that fell due a second ago, or an override that has just begun, counts even if the worker has not recorded it yet. A step of an escalation policy asks the same question in the same way, when it runs.

```
GET /api/v1/schedules                  every schedule, with who is on call
GET /api/v1/schedules/<name>           one schedule
GET /api/v1/schedules/<name>/shifts    who is on call over a period, past or to come
GET /api/v1/teams/<name>/on-call       who is on call for a team
```

All four need a signed-in user or an API token, with either role. They only read: a schedule is provisioned and cannot be changed through the API.

```sh
curl -H "Authorization: Bearer $BEACON_TOKEN" https://beacon.example.com/api/v1/schedules/platform-primary
```

```json
{
  "name": "platform-primary",
  "team": "platform",
  "rotation": {
    "time_zone": "Europe/London",
    "members": ["ada@example.com", "grace@example.com", "linus@example.com"],
    "shift_length": "1w",
    "handoff_time": "09:00",
    "handoff_day": "monday"
  },
  "next_handoff": "2026-12-28T09:00:00Z",
  "on_call": {
    "schedule": "platform-primary",
    "user": "grace@example.com",
    "since": "2026-12-24T09:00:00Z",
    "until": "2026-12-27T09:00:00Z",
    "override": {"id": "0d6f3b52-7a0e-4a0f-9d0b-0a6e5f1f3c77", "user": "grace@example.com", "state": "active", "reason": "covering the holidays", "...": "..."},
    "rotation_user": "ada@example.com"
  }
}
```

| Field | Meaning |
|---|---|
| `rotation` | The rotation in force. It is the one in the files unless a change is pending. |
| `pending` | Present when a changed definition has not taken over yet: its `rotation`, `takes_over_at`, and `stated`, which is `true` when the files state the time and `false` when it is the next handoff |
| `next_handoff` | When the rotation next hands over |
| `on_call.user` | The email address of the person on call, or `null` when nobody is: no rotation member is a user and no override is in force |
| `on_call.since`, `on_call.until` | The shift or the override that puts them on call |
| `on_call.override` | The override in force, when there is one |
| `on_call.rotation_user` | Whom the rotation has on call. It differs from `user` during an override. |

`GET /api/v1/schedules` answers `{"schedules": [...]}` with the same objects, by name. `GET /api/v1/teams/<name>/on-call` answers `{"team": "<name>", "on_call": [...]}` with one `on_call` object per schedule of the team, by schedule name; a team without schedules has an empty list.

### Shifts over a period

`GET /api/v1/schedules/<name>/shifts?from=<time>&to=<time>` answers who is on call from `from` to `to`, both in RFC 3339 form. Without `from` the period begins now, and without `to` it lasts four weeks. It may be at most a year long.

```json
{
  "schedule": "platform-primary",
  "from": "2026-12-21T00:00:00Z",
  "to": "2026-12-29T00:00:00Z",
  "shifts": [
    {"user": "ada@example.com", "starts_at": "2026-12-14T09:00:00Z", "ends_at": "2026-12-21T09:00:00Z", "kind": "rotation", "planned": false},
    {"user": "ada@example.com", "starts_at": "2026-12-21T09:00:00Z", "ends_at": "2026-12-24T09:00:00Z", "kind": "rotation", "planned": true},
    {"user": "grace@example.com", "starts_at": "2026-12-24T09:00:00Z", "ends_at": "2026-12-27T09:00:00Z", "kind": "override", "override_id": "0d6f3b52-7a0e-4a0f-9d0b-0a6e5f1f3c77", "planned": true},
    {"user": "ada@example.com", "starts_at": "2026-12-27T09:00:00Z", "ends_at": "2026-12-28T09:00:00Z", "kind": "rotation", "planned": true},
    {"user": "linus@example.com", "starts_at": "2026-12-28T09:00:00Z", "ends_at": "2026-01-04T09:00:00Z", "kind": "rotation", "planned": true}
  ]
}
```

- **One person at any moment.** Where an override covers the rotation, only the override is listed, and the rotation shift around it is cut. Time in which nobody is on call is left out.
- **Past time comes from the record**, future time from the same arithmetic that will carry it out, with the overrides that exist and a pending change of definition taken into account. `planned` is `false` for a shift that is over and on record, and `true` for one that has not ended: it is what happens if nothing but time changes.
- A shift that reaches beyond the period is listed whole, with its real start and end.

| Status | Meaning |
|---|---|
| `400` | `from` or `to` is not a time, or the period is empty, backwards or longer than a year. `field` names the parameter. |
| `401` | No valid session or token |
| `404` | No schedule, or no team, has this name |

## When a schedule leaves the configuration

Its shift in progress ends at that moment and its position is dropped. The shifts it recorded are kept. If a schedule with the same name returns, it is the same schedule with the same history, and its rotation starts afresh with the first member.
