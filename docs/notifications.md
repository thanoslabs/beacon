---
title: Notifications
tags: [notifications, design]
date: 2026-11-16
---

Beacon does not send a notification at the moment something happens. It writes it to a queue in Postgres, in the same transaction as the change that caused it, and workers deliver from the queue. This page describes what that guarantees. The tables are described in [[configuration-and-data-model]].

## What is queued

A notification is one message about one alert group to one destination, through one integration. Each has a reason:

| Reason | Queued when |
|---|---|
| `escalation_step` | Someone is asked to respond; for now, the first notification of a group |
| `group_update` | The group changed: an alert joined it or resolved, or someone acknowledged, snoozed or resolved alerts, or a snooze ended |
| `resolved` | Every alert of the group is resolved |

The message is built when it is delivered, from the state of the group at that moment. A notification that is delivered late therefore never shows an old state, and a change made while a notification is still waiting in the queue is shown by that notification and gets none of its own.

> [!note] Who is notified
> Routing and escalation policies will decide who is notified. Until they exist, the first notification of every group goes to the same fixed destinations: the channels in the first step of the default escalation policy. Updates go to every destination that was told about the group before.

## Groups

Notifications are about a group of alerts, not about each alert; [[configuration#Grouping]] says how groups are formed. A group has one message per destination, which is updated in place where the service allows it. When that message is sent and updated depends on who caused the change:

| What happens | When it is told |
|---|---|
| A new group opens | After `group_wait` (30 seconds by default), with every alert that has joined by then |
| An alert joins a group that was told about, or a source resolves one | When `group_interval` (5 minutes by default) has passed since the last notification caused by a source: at once if it has, otherwise then. Everything that happened in between is in the one update. |
| A source resolves the last alert | The same; the notification has the reason `resolved` |
| Someone acknowledges, snoozes or resolves, or a snooze ends | At once. A person who presses a button sees the result. |
| A source delivers an alert that is already open again | Never. Repeats are not news. |

- **A group that resolves during its wait tells nobody.** A problem that came and went within `group_wait` produces no notification at all. The alerts and their timelines are stored.
- **An alert that joins an acknowledged group fires.** It is new, and nobody has taken it; the message shows the group as firing again, with one alert firing and the others acknowledged.
- **Acting on a message acts on the group.** *Acknowledge* acknowledges every alert of the group that can be acknowledged, and so on. Fifty alerts acknowledged at once are one update.
- **The timers are rows in Postgres**, not timers in a process: `alert_groups.notify_at` holds when the next notification of a group is due. Every replica looks for due groups once a second, and a group is handed to exactly one of them. A restart loses nothing: whatever became due while no process ran is sent when one starts.

## Delivery

- **Durable.** A queued notification survives a restart, and exists exactly when the change that caused it was committed.
- **Several workers.** Each process runs `--notification-workers` workers (default 4), and several replicas can run at once. A worker claims a notification before it sends it, and no other worker takes a claimed notification.
- **In order per destination.** Notifications about one group to one destination are delivered one at a time, oldest first. An update never overtakes the message it updates. Other groups are not held up.
- **Updates in place.** When a channel returns a reference to the message it delivered, later notifications about the same group to the same destination are given that reference, so a channel that can edit a message does so and sends nothing new.
- **At least once.** If a process dies after the service accepted a message but before Beacon recorded it, the message is sent again once the claim runs out, about 45 seconds later. A message is never lost this way, but it can be delivered twice. On a normal shutdown, deliveries in progress finish first.

## Retries

A failed attempt is retried after a wait that doubles each time: about 5 s, 10 s, 20 s and so on, up to 5 minutes, each shortened a little at random. After 8 attempts, about ten minutes in all, the notification is marked `failed` and is not tried again.

| Failure | What happens |
|---|---|
| The service cannot be reached, times out or reports a temporary error | Retried with the waits above |
| The service says how long to wait, for example when rate limiting | Retried no sooner than it asked |
| The service reports something retrying cannot fix, such as a destination that does not exist | Marked `failed` at once |

One attempt may take at most 15 seconds.

## Tracking

Every attempt is recorded with its time, its outcome (`success`, `retryable_error` or `permanent_error`) and the error. Each attempt also appears on the timeline of the alert (see [[alert-lifecycle]]):

| Event | Written when |
|---|---|
| `notification_sent` | An attempt delivered the message |
| `notification_failed` | An attempt failed. The event holds the error and whether another attempt will follow. |

Both hold the destination, the reason and the attempt number, and their actor is `system`.
