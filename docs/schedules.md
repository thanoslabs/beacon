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
> Beacon runs the rotation and records the shifts. A change to the definition of a schedule that is already running is not picked up yet, there are no overrides, and escalation policies do not page the person on call yet.

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

## When a schedule leaves the configuration

Its shift in progress ends at that moment and its position is dropped. The shifts it recorded are kept. If a schedule with the same name returns, it is the same schedule with the same history, and its rotation starts afresh with the first member.
