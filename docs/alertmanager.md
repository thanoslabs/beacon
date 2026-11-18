---
title: Alertmanager
tags: [ingestion, guide]
date: 2026-11-18
---

Beacon can sit behind an existing [Alertmanager](https://prometheus.io/docs/alerting/latest/alertmanager/): Prometheus and its rules stay as they are, and Alertmanager sends its notifications to Beacon through its webhook receiver. This page sets that up and describes how the two work together. What happens to an alert once Beacon has it is described in [[alert-lifecycle]].

## Set up the source

Define a source of type `alertmanager` in Beacon's configuration (see [[configuration#Sources]]):

```yaml
sources:
  - name: alertmanager
    type: alertmanager
    token: {env: BEACON_SOURCE_TOKEN}
    severity:
      label: severity
      mapping:
        page: critical
        critical: critical
        warning: warning
        info: info
      default: warning
```

The token is never written in the file. Choose a long random one, at least 16 characters:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
```

Beacon has three severity levels, `critical`, `warning` and `info`. The `severity` block says which label of your alerts carries the severity and how its values map to them. Without it, the `severity` label is read as it is, and anything else is a `warning`.

## Set up the receiver

Add a webhook receiver to Alertmanager and route to it. The URL ends in the name of the source.

```yaml
route:
  receiver: beacon
  group_by: [alertname]
  group_wait: 10s
  group_interval: 1m
  repeat_interval: 1h

receivers:
  - name: beacon
    webhook_configs:
      - url: https://beacon.example.com/api/v1/ingest/alertmanager
        send_resolved: true
        max_alerts: 0
        http_config:
          authorization:
            type: Bearer
            credentials_file: /etc/alertmanager/beacon-token
```

The file named by `credentials_file` holds the token of the source. `credentials: <token>` puts it in the configuration instead.

| Setting | Recommended | Why |
|---|---|---|
| `send_resolved` | `true` (the default for webhooks) | It is how Beacon learns that an alert is over. Without it, alerts in Beacon stay open until someone resolves them by hand. |
| `max_alerts` | `0` (the default) | Alerts that Alertmanager leaves out of a notification never reach Beacon. Beacon logs a warning when a notification says alerts were left out. |
| `group_wait` | Short, such as `10s` | How long a new alert waits in Alertmanager before Beacon hears of it |
| `group_interval` | Short, such as `1m` | How long a change in a group that was already sent, a new alert or a resolved one, waits before Beacon hears of it |
| `repeat_interval` | `1h` to `4h` | A repeat costs nothing in Beacon and notifies nobody; see below |
| `timeout` | Unset, or at least `10s` | Beacon stores each alert of a notification before it answers |

To let Beacon take over notifications gradually, keep your other receivers and add `continue: true` to a route that sends to Beacon as well.

## Try it locally

`compose.yaml` has an Alertmanager that is set up this way, under a profile:

```sh
BEACON_CONFIG_DIR=./examples/alertmanager docker compose --profile alertmanager up --build -d --wait

docker compose exec alertmanager amtool alert add DiskFull instance=db1 severity=page \
  --annotation=summary='"Disk is full on db1"' --alertmanager.url=http://localhost:9093
```

About ten seconds later, Beacon logs `alerts received` with `"created":1`. [`examples/alertmanager/beacon.yaml`](https://github.com/thanoslabs/beacon/blob/main/examples/alertmanager/beacon.yaml) is Beacon's side and [`dev/alertmanager/alertmanager.yml`](https://github.com/thanoslabs/beacon/blob/main/dev/alertmanager/alertmanager.yml) is Alertmanager's.

## What Beacon reads

Beacon reads version `4` of Alertmanager's [webhook payload](https://prometheus.io/docs/alerting/latest/configuration/#webhook_config), as Alertmanager 0.35 sends it. One notification holds every alert of one of Alertmanager's groups, and Beacon handles each alert on its own:

| In the payload | In Beacon |
|---|---|
| `alerts[].status` | `firing` opens the alert or refreshes it; `resolved` resolves it |
| `alerts[].fingerprint` | The identity of the alert within the source. When it is missing, as before Alertmanager 0.17, the identity is computed from the labels. |
| `alerts[].labels` | The alert's labels. The source's own `labels` are added where the alert does not have them. |
| `alerts[].annotations` | The alert's annotations. Notifications look for `summary`, `description` and `runbook_url`. |
| `alerts[].startsAt` | When the alert began |
| `alerts[].endsAt` | When a resolved alert ended. It is ignored for a firing alert. |
| `alerts[].generatorURL` | Kept as the alert's `generator` link and offered in notifications as *Source*, when it is an `http` or `https` address of at most 2048 characters |
| `truncatedAlerts` | Logged as a warning when it is not zero |

Everything else in the payload repeats what the alerts say, or describes Alertmanager's own grouping, and is not read. A field Beacon does not know is ignored, so a newer Alertmanager that adds one keeps working. A payload with another `version` is read the same way and logged as a warning. A custom `payload` template in the receiver is not supported: Beacon needs the default payload.

The body may be up to 4 MiB.

## Responses

| Status | Meaning |
|---|---|
| `200` | Every alert in the notification was stored. The body counts them. |
| `400` | The payload is not Alertmanager's. `error` says why and `field` names the part at fault, such as `alerts[1].labels`. Nothing was stored. Alertmanager does not retry this. |
| `401` | The source does not exist or the token is wrong |
| `413` | The body is larger than 4 MiB |
| `500` | Beacon could not store every alert. Alertmanager retries, and the alerts that were stored are recognised when it does. |

```json
{"result": "accepted", "received": 3, "created": 1, "updated": 1, "resolved": 1, "not_found": 0}
```

`not_found` counts resolved alerts that were not open in Beacon: resolved by hand already, or never seen firing.

## How Alertmanager and Beacon work together

Alertmanager groups, repeats and retries on its own schedule. Beacon is built to be on the receiving end of that:

- **Two rounds of grouping.** Alertmanager groups alerts into notifications, and Beacon groups the alerts it receives by its own `group_by`, whatever notification they came in ([[configuration#Grouping]]). The two need not agree. Alertmanager has waited its `group_wait` before it sends, so Beacon's can be short: `group_wait: 5s` in Beacon's `routing` block is enough for every alert of one notification to be stored before anyone is told.
- **Repeats.** Alertmanager sends a group again every `repeat_interval` while it fires, and again when a delivery failed on its side. Beacon recognises each alert by its fingerprint, updates its labels, annotations and the time it was last received, and does nothing else. A repeat never opens a second alert and never sends a notification.
- **Mixed notifications.** A notification can hold firing and resolved alerts together. Each is handled by its own status; the status of the notification as a whole is not used.
- **Resolved alerts are sent once.** Alertmanager includes a resolved alert in one successful notification and then forgets it. If Beacon cannot be reached, Alertmanager retries, gives up after one `group_interval` and sends the notification again at the next, so an outage of Beacon delays a resolution and does not lose it. If the resolved notification is lost for good, for example because the alert expired in Alertmanager during a long outage or the receiver had `send_resolved: false` at the time, the alert stays open in Beacon. Resolve it by hand, for example with the *Resolve* button of its Slack message.
- **Resolved by hand while still firing.** Resolving an alert in Beacon does not resolve it in Alertmanager. If the problem is still there, the next repeat opens a new alert in Beacon, which is notified like any new alert. To quieten an alert that is still firing, acknowledge or snooze it instead.
- **A problem that returns.** An alert that resolved and fires again is a new alert in Beacon, with its own timeline.
- **Order.** Alertmanager sends the notifications about one group one after another, which is the order Beacon needs. Two Alertmanagers of a high-availability cluster should send to the same source, with the same token: they produce the same fingerprints, so Beacon sees one alert, whichever of them delivers.
- **Silences and inhibition in Alertmanager** apply before Beacon hears of anything. An alert that is silenced there is not sent, and Beacon does not know it exists.
