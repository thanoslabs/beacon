---
title: Metrics
tags: [operations, metrics, reference]
date: 2026-11-23
---

Beacon reports on itself in the Prometheus text format at `GET /metrics`: how the process is running and how it answers requests. The same Prometheus that produces your alerts can watch the thing that pages you.

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

A reader of `/metrics` learns the **names** that the configuration files give to sources, integrations, teams and schedules, the routes of the API, and how much of everything there is.

They do not learn anything an alert carried or anything about a person. No metric has a label for an alert's name, its labels, its fingerprint or an identifier; none names a user, an email address, a chat account, a channel or a webhook address; none holds a credential. That is a rule of the code, not a habit: the labels a metric may have are an allow-list ([Labels](#labels)), and a test fails when a metric gains another.

## Labels are bounded

Every label value comes from a set that cannot grow with traffic: a list that is fixed in the code (an outcome, a status code, the pattern of a route) or a name from the configuration files. An install with ten sources has ten values of `source` however many alerts arrive.

One rule follows from that: **requests are counted by the route that took them**, such as `/api/v1/alerts/{id}`, never by the address that was asked for. Addresses that no route takes are counted together as `unmatched`.

A name that left the configuration stays in the counters of a running process until it restarts.

## Several replicas

Each replica serves its own `/metrics`, and what it reports is what that one process did. Add the replicas up: `sum by (route) (rate(beacon_http_requests_total[5m]))`.

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

### HTTP and API

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_http_requests_total` | counter | `route`, `method`, `code` | replica | HTTP requests answered, by the route that took them, its method and the status code. |
| `beacon_http_request_duration_seconds` | histogram | `route`, `method` | replica | Time from the arrival of an HTTP request to the end of its answer, in seconds, by the route that took it and its method. Buckets from 5 ms to 10 s. |

### Ingestion

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
| `beacon_routing_unmatched_alerts_total` | counter | `source` | replica | Alerts opened that matched no routing rule and took the routing default, by source. Repeated deliveries of an open alert are not counted. |

### People

| Metric | Type | Labels | Scope | Meaning |
|---|---|---|---|---|
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
| `result` | A fixed set of outcomes, listed with the metric. |
| `route` | Pattern of the route that took the request, such as `/api/v1/alerts/{id}`, never the address that was asked for; `unmatched` when no route did. |
| `schedule` | Name of a schedule in the configuration. |
| `source` | Name of a source in the configuration. |
| `team` | Name of a team in the configuration. |
| `version` | Version of the binary. |
