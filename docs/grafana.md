---
title: Grafana alerting
tags: [ingestion, guide]
date: 2026-11-18
---

Beacon can receive the alerts of [Grafana alerting](https://grafana.com/docs/grafana/latest/alerting/) through a webhook contact point: the alert rules stay in Grafana, and Grafana sends its notifications to Beacon. This page sets that up. What happens to an alert once Beacon has it is described in [[alert-lifecycle]].

If your Grafana rules send to an external Alertmanager, connect that Alertmanager instead; see [[alertmanager]].

## Set up the source

Define a source of type `grafana` in Beacon's configuration (see [[configuration#Sources]]):

```yaml
sources:
  - name: grafana
    type: grafana
    token: {env: BEACON_SOURCE_TOKEN}
    severity:
      label: severity
      default: warning
```

The token is never written in the file. Choose a long random one, at least 16 characters:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
```

Grafana has no severity of its own. Beacon reads one of its three levels, `critical`, `warning` or `info`, from a label of the alert rule: give your rules a `severity` label, or name another label and map its values with `severity.mapping`.

## Set up the contact point

In Grafana, open **Alerts & IRM → Alerting → Notification configuration → Contact points**, add a contact point and choose the **Webhook** integration:

| Setting | Value |
|---|---|
| URL | `https://beacon.example.com/api/v1/ingest/grafana`: the address of Beacon, ending in the name of the source |
| HTTP Method | `POST` |
| Authentication Header Scheme | `Bearer` |
| Authentication Header Credentials | The token of the source |
| Max Alerts | `0`: every alert of a group, so Beacon knows about all of them |
| Disable resolved message | Off. It is how Beacon learns that an alert is over. |

Leave **Title**, **Message** and **Custom Payload** as they are: Beacon reads the default payload and does not use the title or the message. Do not put the token in the URL, which Grafana stores unprotected.

Then make a notification policy send to the contact point: the default policy, or a nested one for the alerts Beacon should have.

The same as a provisioning file, which is how [`dev/grafana`](https://github.com/thanoslabs/beacon/tree/main/dev/grafana) sets it up:

```yaml
apiVersion: 1
contactPoints:
  - orgId: 1
    name: beacon
    receivers:
      - uid: beacon-webhook
        type: webhook
        disableResolveMessage: false
        settings:
          url: https://beacon.example.com/api/v1/ingest/grafana
          httpMethod: POST
          authorization_scheme: Bearer
          authorization_credentials: $BEACON_SOURCE_TOKEN
          maxAlerts: 0
policies:
  - orgId: 1
    receiver: beacon
    group_by: [grafana_folder, alertname]
    group_wait: 10s
    group_interval: 1m
    repeat_interval: 1h
```

Grafana replaces `$BEACON_SOURCE_TOKEN` with the environment variable of that name when it reads the file.

| Policy setting | Recommended | Why |
|---|---|---|
| Group wait | Short, such as `10s` | How long a new alert waits in Grafana before Beacon hears of it |
| Group interval | Short, such as `1m` | How long a change in a group that was already sent, a new alert or a resolved one, waits before Beacon hears of it |
| Repeat interval | `1h` to `4h` | A repeat costs nothing in Beacon and notifies nobody |

## Try it locally

`compose.yaml` has a Grafana that is set up this way, under a profile. It has the contact point and the policy and no alert rule, so create one that fires at once:

```sh
BEACON_CONFIG_DIR=./examples/grafana docker compose --profile grafana up --build -d --wait

curl -u admin:admin -X POST http://localhost:3000/api/v1/provisioning/alert-rules \
  -H 'Content-Type: application/json' -H 'X-Disable-Provenance: true' \
  -d @dev/grafana/demo-rule.json
```

Within about a minute, Beacon logs `alerts received` with `"created":1`. A Grafana that has just started takes up to a minute before it uses a provisioned contact point. To resolve the alert, pause the rule *HighLatency* in Grafana, at <http://localhost:3000> (sign in as `admin` with the password `admin`), or with:

```sh
jq '.isPaused = true' dev/grafana/demo-rule.json | curl -u admin:admin -X PUT \
  http://localhost:3000/api/v1/provisioning/alert-rules/beacon-demo \
  -H 'Content-Type: application/json' -H 'X-Disable-Provenance: true' -d @-
```

## What Beacon reads

Beacon reads version `1` of the payload of Grafana's [webhook contact point](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/), as Grafana 13.3 sends it. The payload has the shape of Alertmanager's, with more on each alert. One notification holds every alert of a group, and Beacon handles each alert on its own:

| In the payload | In Beacon |
|---|---|
| `alerts[].status` | `firing` opens the alert or refreshes it; `resolved` resolves it |
| `alerts[].fingerprint` | The identity of the alert within the source |
| `alerts[].labels` | The alert's labels, which include `alertname` (the name of the rule) and `grafana_folder`. The source's own `labels` are added where the alert does not have them. |
| `alerts[].annotations` | The alert's annotations. Notifications look for `summary`, `description` and `runbook_url`. |
| `alerts[].startsAt` | When the alert began |
| `alerts[].endsAt` | When a resolved alert ended |
| `alerts[].generatorURL` | The alert's `generator` link: the alert rule in Grafana. Offered in notifications as *Source*. |
| `alerts[].dashboardURL`, `alerts[].panelURL` | The alert's `dashboard` and `panel` links. Grafana sends them for a rule that is linked to a dashboard panel, and empty otherwise. Offered in notifications as *Dashboard* and *Panel*. |
| `alerts[].silenceURL` | The alert's `silence` link: the page in Grafana that silences it there |
| `truncatedAlerts` | Logged as a warning when it is not zero |

A link is kept when it is an `http` or `https` address of at most 2048 characters. The links start with the address Grafana believes it has, its `root_url`; set that correctly, or they point at `localhost`. The dashboard and panel links carry a time range, so they are replaced each time the alert is delivered and keep the last range once it resolves.

The measured values (`values`, `valueString`), the image, `title`, `message` and everything about Grafana's own grouping are not kept. A field Beacon does not know is ignored, so a newer Grafana that adds one keeps working, and a payload with another `version` is read the same way and logged as a warning.

The body may be up to 4 MiB. The responses are the ones described for [[alertmanager#Responses]].

## How Grafana and Beacon work together

Grafana's notifications come from an Alertmanager that is built into it, so what [[alertmanager#How Alertmanager and Beacon work together]] says applies here too: repeats change nothing, firing and resolved alerts may arrive together, and a resolved alert is sent once. In addition:

- **No data and errors.** When a rule has no data or cannot be evaluated, Grafana sends alerts named `DatasourceNoData` and `DatasourceError`, unless the rule says otherwise. Beacon treats them like any alert.
- **A paused rule** resolves its alerts in Grafana, and Grafana sends the resolution.
- **A rule that is removed while Grafana restarts** never sends a resolution: Grafana forgets the alert. The alert then stays open in Beacon; resolve it by hand, for example with the *Resolve* button of its Slack message.
- **Silences and mute timings in Grafana** apply before Beacon hears of anything.
