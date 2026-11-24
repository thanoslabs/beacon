---
title: Metrics
tags: [operations, metrics, reference]
date: 2026-11-23
---

Beacon reports on itself in the Prometheus text format at `GET /metrics`: how the process is running, what it received, what became of the notifications it sent, and how alerts are being handled. The same Prometheus that produces your alerts can watch the thing that pages you.

The page ends with the [reference](#reference) of every metric, which is written from the code.

## Scraping

```yaml
scrape_configs:
  - job_name: beacon
    static_configs:
      - targets: ["beacon:8080"]
```

Scrape every replica, not a load balancer in front of them: most metrics are what one process counted.

### Who can read the metrics

Without further settings, `/metrics` answers anyone who reaches the port, as Prometheus expects of a target. That is right inside a private network and wrong on the internet, so keep the address away from outsiders ([[access#Routes]]).

To make Beacon ask for a credential, give it a token, as a flag or in the environment. A scrape must then send it as a bearer token, and gets `401` without it:

```sh
BEACON_METRICS_TOKEN="$(openssl rand -hex 32)" beacon serve --config /etc/beacon
```

```yaml
scrape_configs:
  - job_name: beacon
    authorization:
      credentials_file: /etc/prometheus/beacon-metrics-token
    static_configs:
      - targets: ["beacon:8080"]
```

The token guards `/metrics` and nothing else. The probes `/healthz` and `/readyz` stay open; they say nothing but whether Beacon is up.

### What the metrics reveal

A reader of `/metrics` learns the **names** that the configuration files give to sources, integrations, teams, schedules, escalation policies and the services of routing rules, the routes of the API, and how much of everything there is: how many alerts a source sent, how many notifications failed.

They do not learn anything an alert carried or anything about a person. No metric has a label for an alert's name, its labels, its fingerprint or an identifier; none names a user, an email address, a chat account, a channel or a webhook address; none holds a credential. `beacon_schedule_covered` says whether somebody is on call for a schedule, never who. That is a rule of the code, not a habit: the labels a metric may have are an allow-list ([Labels](#labels)), and a test fails when a metric gains another.

## Labels are bounded

Every label value comes from a set that cannot grow with traffic: a list that is fixed in the code (an outcome, a status code, the pattern of a route) or a name from the configuration files. An install with ten sources has ten values of `source` however many alerts arrive.

Three rules follow from that:

- **Requests are counted by the route that took them**, such as `/api/v1/alerts/{id}`, never by the address that was asked for. Addresses that no route takes are counted together as `unmatched`.
- **A request to the ingest endpoint counts under a source only when it carried that source's token.** Anyone can ask for any name; requests that are no source's are counted together in `beacon_ingest_unauthorized_requests_total`.

- **A team and a service are labels only by the names the configuration gives them.** An alert's team is the one routing assigned; an alert without one is `none`, and a team that has left the configuration since is `other`. A service is a label when a routing rule names it (`service: postgres`). A rule that names none lets the alert bring its own in a `service` label, and what an alert brings is whatever its sender likes: all such services are counted together as `other`, and an alert without a service as `none`. To see a service by name in the metrics, name it in a routing rule.

A name that left the configuration stays in the counters of a running process until it restarts.

## Alert handling

Three metrics say how alerts are handled, for the reports a team makes of its on-call work.

- `beacon_alerts_opened_total` counts alerts by team, service and severity. A repeated delivery of an alert that is open opens nothing.
- `beacon_alert_time_to_acknowledge_seconds` is the time from the moment Beacon opened an alert to the **first time a person acknowledged or snoozed it**. A snooze counts: it is someone saying "I have seen this". An alert that is resolved before anyone responds has no time to acknowledge, so the histogram's count is also the number of alerts somebody took.
- `beacon_alert_time_to_resolve_seconds` is the time from the same moment to the alert's resolution, by its source or by a person.

Each is observed **once per alert**. An alert that is acknowledged, snoozed, fires again when the snooze runs out and is acknowledged a second time was answered once, the first time. The time is measured from when Beacon opened the alert, not from when its source says the problem began: that is the part Beacon and the people on call can do something about.

Both histograms have buckets from 15 seconds to 24 hours, and are labelled by team and severity, not by service, to keep them small.

```promql
# The time within which nine in ten critical alerts of a team are acknowledged, over a week
histogram_quantile(0.9, sum by (le, team) (rate(beacon_alert_time_to_acknowledge_seconds_bucket{severity="critical"}[7d])))

# The share of alerts that somebody acknowledged or snoozed before they were resolved
sum(increase(beacon_alert_time_to_acknowledge_seconds_count[7d])) / sum(increase(beacon_alerts_opened_total[7d]))

# Alerts that are firing right now, by team (a global metric: max, not sum)
sum by (team) (max without (instance) (beacon_alerts_open{state="firing"}))
```

Escalations are counted by step: `beacon_escalation_steps_total{policy, step, result}`. A step whose every target was skipped ran and paged no one, and is counted with `result="nobody"`; `beacon_escalations_exhausted_total` counts the escalations that ran out with an alert still firing. Both are worth an alert of their own.

## Several replicas

Each replica serves its own `/metrics`, and the reference says for every metric whether it is **replica** or **global**.

- **Replica** metrics are what one process did. Add the replicas up: `sum by (source) (rate(beacon_ingest_alerts_total[5m]))`.
- **Global** metrics are facts about the whole install: the depth of the notification queue, the alerts that are open, whether a schedule has somebody on call. They are read from Postgres, so **every replica reports the same number**. Take one of them, never the sum:

  ```promql
  max by (integration) (beacon_notifications_pending)
  ```

  With `sum`, three replicas would show a queue three times as deep as it is.

Global metrics are read when Beacon is scraped: at most once in five seconds, however many scrapes arrive, and for at most two seconds. When Postgres does not answer in that time, the scrape goes on without them and `beacon_state_metrics_up` is `0`. A value from an earlier read is never served in their place, so a query over a global metric returns nothing while the database is away. Alert on `beacon_state_metrics_up == 0` to hear about that.

## Alerting on Beacon itself

The thing that pages you needs watching by something else. [`dev/prometheus/beacon-alerts.yml`](https://github.com/thanoslabs/beacon/blob/main/dev/prometheus/beacon-alerts.yml) is a set of alerting rules to copy into the Prometheus that scrapes Beacon.

> [!warning] Do not route these alerts through Beacon
> An alert that says Beacon cannot notify anyone must reach you some other way: a receiver of Alertmanager that goes straight to chat or email, not the webhook to Beacon.

| Rule | Fires when |
|---|---|
| `BeaconDown` | A replica has not answered a scrape for two minutes |
| `BeaconCannotReadState` | A replica cannot read the global metrics from Postgres |
| `BeaconConfigurationRejected` | A reload was rejected, and a replica runs a configuration that differs from the files |
| `BeaconNotificationsGivenUp` | A notification was given up: somebody did not get a message |
| `BeaconNotificationsFailing` | Most delivery attempts through an integration fail |
| `BeaconNotificationQueueStuck` | The oldest notification has waited more than five minutes |
| `BeaconNotificationQueueGrowing` | More than 100 notifications wait |
| `BeaconSourceSilent` | A source has delivered nothing for over an hour |
| `BeaconIngestFailing` | Beacon cannot store what a source delivers |
| `BeaconIngestRejected` | Beacon refuses what a source delivers as invalid |
| `BeaconEscalationUnanswered` | Every step of a policy ran and nobody responded |
| `BeaconEscalationStepReachedNobody` | A step ran and every target was skipped |
| `BeaconScheduleNotCovered` | A schedule has had nobody on call for 15 minutes |
| `BeaconAPIErrors` | More than 5% of requests end in a `5xx` answer |

Three of them are the dead man's switches of the pipeline. **Notifications failing** and **the queue growing** say that alerts come in and do not go out. **A source gone silent** says that they do not come in: `BeaconSourceSilent` fires when a source that Beacon knows has delivered nothing for an hour. That only means something for a source that always has something to say. The usual way to get one is an always-firing alert in Prometheus, which Alertmanager repeats to Beacon at its `repeat_interval`; kube-prometheus calls it `Watchdog`:

```yaml
- alert: Watchdog
  expr: vector(1)
  labels: {severity: info}
```

Set the window of the rule to a little more than that interval, route the `Watchdog` alert in Beacon to a policy that notifies a quiet channel, and limit the rule to such sources with a matcher on `source` if your others are silent when all is well.

The thresholds are starting points. The rules are checked and unit-tested with `promtool` in Beacon's CI ([`beacon-alerts.test.yml`](https://github.com/thanoslabs/beacon/blob/main/dev/prometheus/beacon-alerts.test.yml)), against the Prometheus version pinned in `compose.yaml`.

## Dashboard

[`dev/grafana/dashboards/beacon.json`](https://github.com/thanoslabs/beacon/blob/main/dev/grafana/dashboards/beacon.json) is a Grafana dashboard of all of the above: what is firing now, the queue, ingestion by source, time to acknowledge and to resolve by team, escalations, deliveries by integration, and the API. Import it in Grafana (Dashboards, New, Import) and choose your Prometheus data source; nothing in it belongs to one install.

Its panels read global metrics with `max without (instance)`. If your replicas differ in another label, such as `pod`, add it there.

To see it with data, start Beacon with the Grafana of the Compose file, which now brings a Prometheus that scrapes Beacon and evaluates the rules:

```sh
BEACON_CONFIG_DIR=./examples/grafana docker compose --profile grafana up --build -d --wait
```

Grafana is at `http://localhost:3000` (`admin` and `admin`), with the dashboard in the folder Beacon, and Prometheus at `http://localhost:9090`. Set `BEACON_GRAFANA_PORT` or `BEACON_PROMETHEUS_PORT` if a port is taken.

## Reference

Every metric Beacon exposes under the prefix `beacon_`. This part of the page is written from the catalogue in `internal/metrics`, and a test fails when the two differ: do not edit it, run `go test ./internal/metrics -run TestReferenceIsTheCatalogue -update`.

"Replica" is what one process did or holds; "global" is a fact about the whole install, read from Postgres, which every replica reports alike ([Several replicas](#several-replicas)).

### Process and configuration

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_build_info` | gauge | `version`, `commit`, `goversion` | replica | Build information for the running Beacon binary. The value is always 1. |
| `beacon_config_reloads_total` | counter | `result` | replica | Configuration reloads since the process started, by result: success or failure. Loading the configuration at start is not counted. |
| `beacon_config_last_reload_successful` | gauge | none | replica | Whether the last attempt to load the configuration succeeded: 1 or 0. When 0, the configuration loaded before is still active. |
| `beacon_config_last_reload_success_timestamp_seconds` | gauge | none | replica | Unix time at which the active configuration was loaded. |
| `beacon_state_metrics_up` | gauge | none | replica | Whether this process could read the global metrics from Postgres for this scrape: 1 or 0. When 0, the metrics that are read from Postgres are absent from the scrape rather than stale. |
| `beacon_state_metrics_read_duration_seconds` | gauge | none | replica | How long the last read of the global metrics from Postgres took, in seconds. The read is abandoned after two seconds. |

### HTTP and API

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_http_requests_total` | counter | `route`, `method`, `code` | replica | HTTP requests answered, by the route that took them, its method and the status code. |
| `beacon_http_request_duration_seconds` | histogram | `route`, `method` | replica | Time from the arrival of an HTTP request to the end of its answer, in seconds, by the route that took it and its method. Buckets from 5 ms to 10 s. |

### Ingestion

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_ingest_requests_total` | counter | `source`, `result` | replica | Requests to the ingest endpoint that carried the token of a source, by source and result: accepted, invalid (the payload was refused) or error (an alert could not be stored, and the sender is asked to send again). |
| `beacon_ingest_unauthorized_requests_total` | counter | none | replica | Requests to the ingest endpoint that named no source of the configuration or carried a wrong token. The name that was asked for is not a label: anyone can ask for any name. |
| `beacon_ingest_alerts_total` | counter | `source`, `result` | replica | Alerts in accepted deliveries, by source and by what became of them: created (a new alert), updated (a repeat of an open alert), resolved, or not_found (a resolution of an alert Beacon does not have open). |
| `beacon_routing_unmatched_alerts_total` | counter | `source` | replica | Alerts opened that matched no routing rule and took the routing default, by source. Repeated deliveries of an open alert are not counted. |

### Alert handling

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_alerts_opened_total` | counter | `team`, `service`, `severity` | replica | Alerts opened, by the team and the service that routing assigned and by severity. A repeated delivery of an open alert opens nothing and is not counted. |
| `beacon_alert_time_to_acknowledge_seconds` | histogram | `team`, `severity` | replica | Time from the moment Beacon opened an alert to the first time a person acknowledged or snoozed it, in seconds. Observed once per alert; an alert that is resolved before anyone responds is not in it. Buckets from 15 s to 24 h. |
| `beacon_alert_time_to_resolve_seconds` | histogram | `team`, `severity` | replica | Time from the moment Beacon opened an alert to its resolution, by its source or by a person, in seconds. Observed once per alert. Buckets from 15 s to 24 h. |
| `beacon_alerts_open` | gauge | `team`, `severity`, `state` | global | Alerts that are not resolved, by team, severity and state. |

### Escalation

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_escalation_steps_total` | counter | `policy`, `step`, `result` | replica | Escalation steps run, by policy, by the number of the step and by result: notified (at least one target was told) or nobody (every target was skipped, so the step paged no one). |
| `beacon_escalations_exhausted_total` | counter | `policy` | replica | Escalations that ended because every step had run, as often as the policy repeats, while an alert was still firing: nobody responded. An escalation whose policy left the configuration while it ran ends the same way and is counted under the policy `other`. |

### Notifications

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_notification_attempts_total` | counter | `integration`, `type`, `outcome` | replica | Delivery attempts, by integration, its type and the outcome. A retryable_error is tried again; a permanent_error is not. |
| `beacon_notifications_failed_total` | counter | `integration`, `type` | replica | Notifications that were given up: the failure was permanent, or every attempt failed. Each is a message somebody did not get. |
| `beacon_notification_send_duration_seconds` | histogram | `type` | replica | Time one delivery attempt took, in seconds, by the type of the integration, whatever its outcome. Buckets from 5 ms to 10 s. |
| `beacon_notifications_pending` | gauge | `integration` | global | Notifications in the queue that are neither delivered nor given up, by integration: the queue depth. It includes those that wait for their next attempt. |
| `beacon_notifications_oldest_pending_age_seconds` | gauge | none | global | Age in seconds of the oldest notification in the queue that is neither delivered nor given up; 0 when the queue is empty. |

### People

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_schedule_covered` | gauge | `schedule` | global | Whether somebody is on call for the schedule now, by the rotation or by an override: 1, or 0 during a gap in coverage. It says that someone is on call, never who. |
| `beacon_team_members_skipped` | gauge | `team` | replica | Email addresses in a team's members list that belong to no user, by team, as of the last time this process applied the configuration or completed a sign-in. Such a person joins the team when they first sign in. |
| `beacon_schedule_members_skipped` | gauge | `schedule` | replica | Email addresses in a schedule's rotation that belong to no user, by schedule, as of the last time this process applied the configuration. Such a member is passed over until the person has signed in. |
| `beacon_users_without_identity` | gauge | `integration` | replica | Users who have no linked account in the chat service of an integration, by integration, as of the last time this process looked. Such a person cannot be reached directly through that integration. |

### Labels

The only labels a Beacon metric can have. A test fails when a metric gains one that is not in this list.

| Label | Where its values come from |
|---|---|
| `code` | HTTP status code of the answer. |
| `commit` | Commit the binary was built from. |
| `goversion` | Go version the binary was built with. |
| `integration` | Name of an integration in the configuration. |
| `method` | HTTP method of the route. |
| `outcome` | Outcome of a delivery attempt: `success`, `retryable_error` or `permanent_error`. |
| `policy` | Name of an escalation policy in the configuration. |
| `result` | A fixed set of outcomes, listed with the metric. |
| `route` | Pattern of the route that took the request, such as `/api/v1/alerts/{id}`, never the address that was asked for; `unmatched` when no route did. |
| `schedule` | Name of a schedule in the configuration. |
| `service` | Name of a service that a routing rule of the configuration names. A service that an alert brought in its own `service` label is `other`, and an alert without a service is `none`. |
| `severity` | `critical`, `warning` or `info`. |
| `source` | Name of a source in the configuration. |
| `state` | State of an alert that is not resolved: `firing`, `acknowledged` or `snoozed`. |
| `step` | Number of a step in its escalation policy, from 1. The configuration says how many steps a policy has. |
| `team` | Name of a team in the configuration; `none` for an alert that no team was assigned, and `other` for a team that has left the configuration. |
| `type` | Type of the integration: `slack`, `discord`, `email` or `webhook`. |
| `version` | Version of the binary. |
