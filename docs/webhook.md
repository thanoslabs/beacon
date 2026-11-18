---
title: Generic JSON webhook
tags: [ingestion, reference]
date: 2026-11-16
---

Any system that can send JSON over HTTP can send alerts to Beacon through a webhook source. For Alertmanager, use an `alertmanager` source instead, which reads Alertmanager's own notifications; see [[alertmanager]]. One request reports one alert as firing, or as resolved. What happens to the alert afterwards is described in [[alert-lifecycle]].

## Set up a source

A source has a name and a token, and is defined in the configuration files (see [[configuration]]):

```yaml
sources:
  - name: default
    type: webhook
    token: {env: BEACON_SOURCE_TOKEN}
```

The token is never written in the file. The file names an environment variable, or a file, that holds it. Choose a long random token, at least 16 characters, for example:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
```

A configuration may define as many sources as you like, each with its own token. A source can also add labels to its alerts and say how severity is derived; see [[configuration#Sources]].

## Endpoint

```
POST /api/v1/ingest/<source name>
Authorization: Bearer <source token>
Content-Type: application/json
```

Each source accepts only its own token. The body is limited to 1 MiB.

## Payload

```json
{
  "status": "firing",
  "labels": {
    "alertname": "DiskFull",
    "host": "db1",
    "severity": "critical"
  },
  "annotations": {
    "summary": "Disk is full on db1",
    "description": "/var/lib/postgresql has 2% free",
    "runbook_url": "https://runbooks.example.com/disk-full"
  },
  "starts_at": "2026-01-02T15:04:05Z"
}
```

| Field | Required | Meaning |
|---|---|---|
| `status` | no | `firing` (the default) or `resolved` |
| `labels` | yes, when firing | Object of string values that identify the alert. At least one label. |
| `annotations` | no | Object of string values that describe the alert. They do not identify it. `summary`, `description` and `runbook_url` are the names notifications look for. |
| `fingerprint` | no | The identity of the alert, up to 256 characters, when the labels are not a good identity. See below. |
| `starts_at` | no | When the problem began, in RFC 3339 form. Defaults to the time of the request. |
| `ends_at` | no | When the problem ended, in RFC 3339 form. Only with `resolved`. Defaults to the time of the request. |

A field that is not in this table is an error, so that a misspelt name is noticed.

### Identity

Beacon recognises a later request as the same alert by its fingerprint. Without a `fingerprint` field, the fingerprint is computed from the labels: the same labels, in any order, are the same alert. Annotations are not part of it.

Send a `fingerprint` when a label changes while the problem continues, such as a measured value. To resolve an alert, send the same labels, or the same `fingerprint`, that fired it.

### Severity

The severity is read from the `severity` label: `critical`, `warning` or `info`. An alert without the label, or with another value, is a `warning`.

## Responses

| Status | `result` | Meaning |
|---|---|---|
| `201` | `created` | A new alert was opened |
| `200` | `updated` | The alert was already open; its labels, annotations, severity and last-received time were refreshed |
| `200` | `resolved` | The open alert was resolved |
| `200` | `not_found` | A `resolved` request matched no open alert. This is not an error, so a retried resolve succeeds. |

A successful response also holds the `alert_id` and its `status`:

```json
{"result": "created", "alert_id": "0f8b1c1e-6a0e-4b0e-9a57-0d7f6d4f3c11", "status": "firing"}
```

| Status | Meaning |
|---|---|
| `400` | The payload is invalid. `error` says why and `field` names the part at fault, for example `labels.port`. |
| `401` | The source does not exist or the token is wrong. The two are not told apart. |
| `413` | The body is larger than 1 MiB |
| `500` | Beacon could not store the alert. Send the request again. |

```json
{"error": "must be a string", "field": "labels.port"}
```

## Example

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
beacon serve --config examples/quickstart &

# Fire
curl -X POST http://localhost:8080/api/v1/ingest/default \
  -H "Authorization: Bearer $BEACON_SOURCE_TOKEN" \
  -d '{"labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"},
       "annotations": {"summary": "Disk is full on db1"}}'

# Resolve
curl -X POST http://localhost:8080/api/v1/ingest/default \
  -H "Authorization: Bearer $BEACON_SOURCE_TOKEN" \
  -d '{"status": "resolved", "labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"}}'
```
