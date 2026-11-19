---
title: Escalation
tags: [escalation, on-call, notifications]
date: 2026-11-18
---

An escalation policy says who is told about an alert, in which order, and how long each of them has to respond before the next is told. Policies are defined in the configuration files ([[configuration#Escalation policies]]), and routing gives every alert group one ([[configuration#How an alert is routed]]).

```yaml
escalation_policies:
  - name: platform-default
    repeat: 2
    steps:
      - notify:
          - schedule: platform-primary
        timeout: 5m
      - notify:
          - team: platform
          - channel: "#platform-alerts"
            integration: slack
        timeout: 10m
      - notify:
          - user: ada@example.com
        timeout: 15m
```

[`examples/on-call`](https://github.com/thanoslabs/beacon/tree/main/examples/on-call) is a complete configuration with a rotation and a policy like this one.

With this policy the person on call for `platform-primary` is paged first. If five minutes pass and an alert of the group is still firing, the whole team and the channel are told. Ten minutes later Ada is. Fifteen minutes after that the policy starts again from its first step, and it does so twice (`repeat: 2`) before it gives up.

## How a group is escalated

| When | What happens |
|---|---|
| The group's first notification is due ([[notifications#Groups]]) | The first step runs: its targets are notified |
| A step's `timeout` has passed and an alert of the group is still firing | The next step runs. After the last step, the first runs again, `repeat` more times in all. |
| No alert of the group is firing any more | Escalation stops, at that moment. Nobody else is told. |
| An alert fires in a group whose escalation had stopped | Escalation starts again from the first step |
| Every step has run, `repeat` + 1 times, and an alert is still firing | Escalation ends. The timeline gets an `escalation_exhausted` event, and the log a warning. |

- **Escalation is about the group**, like every notification. A group of fifty alerts is escalated once.
- **"Still firing" means firing.** An alert that has been acknowledged or snoozed has been taken by someone, and a resolved one is over. Escalation goes on as long as at least one alert of the group is `firing`: acknowledging one alert of two does not stop it, acknowledging the group does.
- **Acknowledging, snoozing and resolving stop it**, whoever does it and from wherever: a person in Slack, the source that resolves the alert. The escalation's timer is cleared in the same database transaction as the change.
- **It starts again when nobody has the alert any more.** When a snooze runs out, or when a new alert joins a group whose alerts had all been taken, an alert is firing that nobody has acknowledged. The policy runs again from its first step, straight away.
- **A step that reaches a destination a second time says the alert again.** In Slack that is a new message, because editing the one that is there notifies nobody; the old message is turned into a pointer to the new one. See [[slack#The message]].

## Targets

| Target | Who is notified |
|---|---|
| `{schedule: <name>}` | Whoever is on call for the schedule at that moment, taking overrides into account ([[schedules#Who is on call]]) |
| `{team: <name>}` | Every member of the team: the ones listed in the files and the ones who belong through an identity provider group ([[access]]) |
| `{user: <email>}` | The user with that email address |
| `{channel: <channel>, integration: <name>}` | A shared destination, such as a Slack channel |

People are resolved when the step runs, not when the alert arrives: a step that runs after a handoff pages the person who is on call by then.

### How a person is reached

A person is notified through an integration that can reach them directly. **Today that is a direct message in Slack**, to the Slack account that Beacon has linked to the user by their email address ([[slack#People]]). Email and Discord will be added as those integrations are.

- Without `via`, a person is reached through every Slack integration in the configuration.
- `via: [<integration>, ...]` on a schedule, team or user target narrows that to the integrations it names.
- The message is the group's message, with the same buttons as in a channel, and it is updated in place like one.

### Who is skipped

A target that cannot be notified is skipped. The step goes on with the others, each skipped target is logged as a warning, `escalation target not notified`, and the step's event on the timeline lists it with the reason:

| Reason | Meaning |
|---|---|
| `the person has no linked account for this integration` | Beacon found no Slack account with the user's email address. See [[slack#People]]; the metric `beacon_users_without_identity` counts such users. |
| `the schedule has nobody on call: a gap in coverage` | No member of the rotation is a user, and no override is in force. A gap is a legitimate state: it is logged as a warning, like every skip, and never as an error. |
| `the team has no members who are users` | |
| `no user has this email address` | The person has never signed in |
| `the integration does not deliver notifications` | The target names an integration of a type that Beacon cannot send through yet, such as email |
| `no integration in the configuration can reach a person directly` | There is no Slack integration |

A step in which everyone was skipped still counts: its timeout runs, and the next step follows. `beacon serve` logs a warning at start and at every reload for a policy none of whose targets can be reached at all.

## Silences

A silence keeps Beacon from telling people about an alert, and that includes telling more people ([[silences]]). When a step is due and every firing alert of the group is silenced, the step waits: it runs when the silence that ends first ends, or at once when that silence is expired. The timeline gets a `notification_silenced` event. A group with one alert silenced and another not goes on escalating.

## Timers

The position of a group in its policy is a row in the database: the policy, the step that ran last, the pass, and when the next step is due.

- **A restart loses nothing.** A step that fell due while no process ran is run by the next one to start.
- **Once, with several replicas.** Every replica looks for due steps once a second. The one that runs a step holds the row and moves its timer in the same transaction, and the others skip it.
- **A reload applies to escalations in progress.** The next step that runs is the next step of the policy as it is then. A timer that is already running keeps the timeout it started with. If the policy has left the configuration, escalation ends with an `escalation_exhausted` event that says so.
- A step that fails to run, for example because the database is unreachable, changes nothing and is tried again thirty seconds later.

## On the timeline

| Event | Written when |
|---|---|
| `escalated` | A step ran. The event holds the policy, the step and how many there are, the pass and how many there are, the timeout, everyone who was `notified` (target, user, integration and destination) and everyone who was `skipped` (target, user, integration and reason). |
| `escalation_exhausted` | Every step ran, as often as the policy repeats, and an alert is still firing; or the policy left the configuration |
| `notification_silenced` | A step waited for a silence. The event holds the silence and when it ends. |
| `not_notified` | The group has no policy to run: the configuration had no `routing` block when it opened, or its policy has left the configuration |

```json
{
  "policy": "platform-default", "step": 2, "steps": 3, "pass": 1, "passes": 3, "timeout": "10m",
  "notified": [
    {"target": "team platform", "user": "grace@example.com", "integration": "slack", "destination": "U0123ABCDEF"},
    {"target": "channel #platform-alerts", "integration": "slack", "destination": "#platform-alerts"}
  ],
  "skipped": [
    {"target": "team platform", "user": "linus@example.com", "integration": "slack", "reason": "the person has no linked account for this integration"}
  ]
}
```

Each notification that a step queues is then delivered, retried and recorded like any other ([[notifications]]).
