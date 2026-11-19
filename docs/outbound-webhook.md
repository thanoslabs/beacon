---
title: Outbound webhook
tags: [notifications, webhook, guide, reference]
date: 2026-11-19
---

Beacon can post what it knows about an alert group to any HTTP endpoint: a ticket system, an automation, a chat service it has no integration for. It sends one request when a group fires, and one for every change after that. This page is the contract for those requests. Delivery, retries and tracking are described in [[notifications]].

The webhook that *receives* alerts is another thing; see [[webhook]].

## Configure Beacon

```yaml
integrations:
  - name: tickets
    type: webhook
    webhook:
      url: https://tickets.example.com/hooks/beacon
      signing_secret: {env: BEACON_TICKETS_SIGNING_SECRET}
      headers:
        X-Team: platform
        Authorization: {env: BEACON_TICKETS_AUTHORIZATION}

escalation_policies:
  - name: default
    steps:
      - notify:
          - integration: tickets
        timeout: 5m

routing:
  default:
    escalation_policy: default
```

| Field | Required | Meaning |
|---|---|---|
| `url` | yes | Where the requests go: an `http` or `https` address |
| `signing_secret` | yes | The key with which every request is signed, at least 16 characters, as a secret reference. Make one with `openssl rand -hex 32`, and give the same value to the receiver. |
| `headers` | no | Headers sent with every request. A value is a plain string, or a secret reference for a credential. The headers Beacon sets itself cannot be configured: `Content-Type`, `User-Agent`, `Idempotency-Key` and everything that starts with `X-Beacon-`. |

**An outbound webhook is named alone in a step**: `{integration: tickets}`, without a `channel`, because its one address is in the integration. It cannot reach a person, so it cannot be named in the `via` of a schedule, team or user target.

[`examples/outbound-webhook`](https://github.com/thanoslabs/beacon/tree/main/examples/outbound-webhook) holds this configuration with a webhook source.

## The request

```
POST /hooks/beacon HTTP/1.1
Content-Type: application/json
User-Agent: Beacon/0.3.0
Idempotency-Key: 6e0c7c5e-2a6f-4c4b-9a5a-0d4d1f6f6b1e
X-Beacon-Reason: escalation_step
X-Beacon-Timestamp: 1772366490
X-Beacon-Signature: v1=5d41402abc4b2a76b9719d911017c592ae2f6b1c0a4e7d3f8b9c6a5e4d3c2b1a
```

| Header | Meaning |
|---|---|
| `Idempotency-Key` | Identifies the notification. The same value is the payload's `id`, and every attempt to deliver one notification carries it. |
| `X-Beacon-Reason` | Why the notification was queued; the payload's `reason` |
| `X-Beacon-Timestamp` | When the request was signed, in seconds since 1970 |
| `X-Beacon-Signature` | The signature; see [Verify a request](#verify-a-request) |

Plus the headers of the configuration.

### When a request is sent

| `reason` | Sent when |
|---|---|
| `escalation_step` | A step of the escalation policy that names the webhook runs: first when the group fires, and again when the policy reaches the webhook once more or a snooze runs out |
| `group_update` | The group changed after that: someone acknowledged, snoozed or resolved alerts, a snooze ended, or an alert joined the group or resolved (at most once per `group_interval`, see [[configuration#Grouping]]) |
| `resolved` | Every alert of the group is resolved |

`reason` says why Beacon sent the request; **`group.status` says what state the group is in**, and is what a receiver should act on: open a ticket on `firing`, note the acknowledgement on `acknowledged`, close it on `resolved`. The payload is built when the request is made, so it never shows a state older than the request.

## The payload

This is version `1`. Fields may be added to it without a new version, so **a receiver must ignore fields it does not know**. A field is removed, or changes its meaning, only with a new `version`.

```json
{
  "version": "1",
  "id": "11111111-2222-4333-8444-555555555555",
  "reason": "group_update",
  "sent_at": "2026-03-01T12:01:30Z",
  "group": {
    "id": "3f0e9a0c-6f6e-4a53-9a5e-0d6a4b0c2a11",
    "status": "acknowledged",
    "severity": "critical",
    "title": "Disk is full on db1",
    "labels": {"alertname": "DiskFull"},
    "counts": {"firing": 0, "acknowledged": 1, "snoozed": 0, "resolved": 0}
  },
  "alerts": [
    {
      "id": "9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90",
      "fingerprint": "fp-1",
      "status": "acknowledged",
      "severity": "critical",
      "labels": {"alertname": "DiskFull", "host": "db1"},
      "annotations": {"summary": "Disk is full on db1", "runbook_url": "https://runbooks.example.com/disk"},
      "links": {"generator": "https://prometheus.example.com/graph?g0.expr=up"},
      "service": "postgres",
      "started_at": "2026-03-01T12:00:00Z",
      "acknowledged_at": "2026-03-01T12:01:00Z",
      "url": "https://beacon.example.com/api/v1/alerts/9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90"
    }
  ],
  "alerts_omitted": 0,
  "last_change": {"type": "acknowledged", "at": "2026-03-01T12:01:00Z", "actor_type": "user", "actor_name": "Ada Lovelace"},
  "paged": [
    {"email": "ada@example.com", "name": "Ada Lovelace", "not_connected": []},
    {"email": "grace@example.com", "name": "Grace Hopper", "not_connected": ["Slack"]}
  ]
}
```

| Field | Type | Meaning |
|---|---|---|
| `version` | string | `"1"` |
| `id` | string | The notification. The same in every attempt to deliver it. |
| `reason` | string | `escalation_step`, `group_update` or `resolved`; see above |
| `sent_at` | time | When this attempt was made |
| `group.id` | string | The alert group. The same in every request about it. |
| `group.status` | string | `firing` while any alert of the group fires, then `acknowledged`, then `snoozed`, and `resolved` once all are |
| `group.severity` | string | `critical`, `warning` or `info`: the highest among the alerts that are not resolved |
| `group.title` | string | What the group is called: for one alert its `summary` annotation, or its `alertname`; for several, the label values they share |
| `group.labels` | object | The labels the alerts of the group share |
| `group.counts` | object | How many alerts are in each state. All four states are always present. |
| `alerts` | array | The alerts of the group, those that fire first, at most 50 |
| `alerts_omitted` | number | How many alerts did not fit |
| `alerts[].id` | string | The alert in Beacon |
| `alerts[].fingerprint` | string | The alert at its source ([[alert-lifecycle#Deduplication and auto-resolve]]) |
| `alerts[].status`, `alerts[].severity` | string | As for the group, for this alert |
| `alerts[].labels`, `alerts[].annotations` | object | As the source sent them, with the labels the source adds in the configuration |
| `alerts[].links` | object | The addresses the source sent, by name: `generator`, `dashboard`, `panel`, `silence` |
| `alerts[].service` | string | The service routing gave the alert. Absent when it has none. |
| `alerts[].started_at` | time | When the alert began to fire |
| `alerts[].acknowledged_at`, `alerts[].snoozed_until`, `alerts[].resolved_at` | time | Absent unless the alert is in that state |
| `alerts[].url` | string | The alert in Beacon's API. Absent when Beacon does not know its own address (`auth.public_url`). |
| `last_change` | object | The latest state change of the alert the group's status is taken from: its `type` (`fired`, `acknowledged`, `snoozed`, `snooze_ended` or `resolved`), when, and who: `actor_type` is `user`, `api_token`, `source` or `system`, and `actor_name` a person's name, `the source` or `Beacon`. Absent when the timeline has none. |
| `paged` | array | The people the escalation policy has paged so far, or meant to. `not_connected` lists the chat services in which a person has no linked account ([[escalation#Who is skipped]]). |

Times are RFC 3339, in UTC. Arrays and objects are never `null`.

## Verify a request

Anyone who learns the address can post to it. The signature is how a receiver knows that a request comes from Beacon and was not changed or kept for later.

The signature is the HMAC-SHA256, under the signing secret, of the text `v1:<timestamp>:<body>`, where `<timestamp>` is the value of `X-Beacon-Timestamp` and `<body>` the request body exactly as it arrived. The header holds it in hexadecimal after `v1=`.

A receiver should:

1. Read the raw body, before parsing it.
2. Compute the HMAC as above and compare it with the header **in constant time**.
3. Refuse a request whose timestamp is more than five minutes from its own clock, which stops a recorded request being sent again later.
4. Keep the `Idempotency-Key` of what it has processed, and answer a repeat with success without acting twice.

```python
import hashlib, hmac, time

def verify(secret: bytes, headers, raw_body: bytes) -> bool:
    timestamp = headers.get("X-Beacon-Timestamp", "")
    if not timestamp.isdigit() or abs(time.time() - int(timestamp)) > 300:
        return False
    signed = b"v1:" + timestamp.encode() + b":" + raw_body
    expected = "v1=" + hmac.new(secret, signed, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, headers.get("X-Beacon-Signature", ""))
```

To change the secret, change it at the receiver and in Beacon's configuration, and reload ([[configuration#Reload without a restart]]). A receiver that accepts either secret for a while loses no request.

## Answers and retries

| The receiver answers | What Beacon does |
|---|---|
| Any `2xx` | Records the delivery. The body of the answer is not read for meaning. |
| Any `3xx` | Gives up at once: **redirects are not followed**. Put the final address in the configuration. |
| `410` | Gives up at once: the receiver says it is gone for good |
| Anything else, a timeout, or no answer | Retries with backoff, up to the limit ([[notifications#Retries]]) |
| A `Retry-After` header with a number of seconds, on any failure | Waits at least that long before the next attempt, and at most an hour |

- **At least once.** A request can arrive twice: when the answer was lost, or when Beacon stopped between the receiver's answer and its own record of it. Both carry the same `Idempotency-Key`. Each attempt is signed afresh, with its own timestamp.
- **In order per group.** Requests about one group are sent one at a time, oldest first. Requests about different groups are not ordered.
- An attempt may take at most 15 seconds.

## What Beacon will not do

The address is in the configuration files, which are as trusted as whoever may change them, and receivers are often internal systems. Beacon therefore sends to private and loopback addresses. To limit what a mistaken or hostile address can reach:

- **Redirects are not followed.** A request goes to the address in the file and nowhere else, and its headers and signature with it.
- **Link-local addresses are refused** (`169.254.0.0/16`, `fe80::/10`), checked on the address the name resolves to at the moment of connecting. That is where cloud platforms serve the credentials of the machine. The attempt fails at once and is not retried.
- **Nothing a receiver answers is stored or shown.** The timeline holds the status code of a failed attempt and nothing of the body, and never the address, which may hold a token.
- Only so much of an answer is read (64 KB), and every attempt has a deadline.

If Beacon should not be able to reach parts of your network at all, restrict that where it runs, with a network policy or a firewall; this list is not a substitute.
