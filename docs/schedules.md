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

> [!warning] What works today
> Beacon runs the rotation, records the shifts and applies changes to a definition safely. There are no overrides yet, and escalation policies do not page the person on call yet.

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
- When no member is a user, **nobody is on call**, and Beacon logs that as an error at every handoff. The first member to sign in is on call from that moment until the handoff that was planned.

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

## When a schedule leaves the configuration

Its shift in progress ends at that moment and its position is dropped. The shifts it recorded are kept. If a schedule with the same name returns, it is the same schedule with the same history, and its rotation starts afresh with the first member.
