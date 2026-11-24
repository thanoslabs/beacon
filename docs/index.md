---
title: Home
---

Project documentation. Every markdown file under `docs/` is built into a static site by [nebula-md](https://github.com/thanoslabs/nebula-md) and published to GitHub Pages on merge to `main` (public repositories only).

Link between pages with Obsidian-style `[[wikilinks]]`.

## Start here

- [[quickstart]]: from `docker compose up` to a test page in Slack or Discord in about five minutes, and what a real install needs beyond that

## Set it up

- [[configuration]]: reference for the YAML configuration files, and how to check them
- [[alertmanager]]: put Beacon behind an existing Alertmanager
- [[grafana]]: send the alerts of Grafana alerting to Beacon
- [[webhook]]: send alerts to Beacon as JSON over HTTP
- [[slack]]: set up the Slack app and send alerts to a channel
- [[discord]]: set up the Discord application and send alerts to a channel
- [[email]]: send alerts as email over SMTP
- [[outbound-webhook]]: post alerts as signed JSON to any HTTP endpoint, and the contract for the payload
- [[access]]: sign in through an OpenID Connect identity provider, users, sessions and API tokens

## How it behaves

- [[alert-lifecycle]]: the states an alert moves through, and its timeline
- [[schedules]]: on-call rotations, handoffs, time zones and recorded shifts
- [[escalation]]: how a policy pages people step by step until someone responds
- [[silences]]: keep Beacon quiet about matching alerts for a period
- [[notifications]]: how notifications are queued, retried and recorded
- [[web]]: the web interface: what it shows, what the browser is given, and how to work on it

## Run it

- [[metrics]]: Beacon's own Prometheus metrics, alerting rules for Beacon itself and a Grafana dashboard
- [[api]]: the REST API: how it behaves as a whole, the routes of alerts and of what the configuration defines, and its OpenAPI description
- [[api-reference]]: every route of the API, generated from the OpenAPI description

## The project

- [[requirements]]: what Beacon must do, split into MVP and later, and the decisions made
- [[configuration-and-data-model]]: the design behind the configuration schema and the Postgres data model
