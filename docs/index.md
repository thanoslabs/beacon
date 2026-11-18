---
title: Home
---

Project documentation. Every markdown file under `docs/` is built into a static site by [nebula-md](https://github.com/thanoslabs/nebula-md) and published to GitHub Pages on merge to `main` (public repositories only).

Link between pages with Obsidian-style `[[wikilinks]]`.

## Pages

- [[requirements]]: what Beacon must do, split into MVP and later
- [[configuration]]: reference for the YAML configuration files, and how to check them
- [[configuration-and-data-model]]: the design behind the configuration schema and the Postgres data model
- [[access]]: sign in through an OpenID Connect identity provider, users and sessions
- [[alert-lifecycle]]: the states an alert moves through, and its timeline
- [[alertmanager]]: put Beacon behind an existing Alertmanager
- [[grafana]]: send the alerts of Grafana alerting to Beacon
- [[webhook]]: send alerts to Beacon as JSON over HTTP
- [[schedules]]: on-call rotations, handoffs, time zones and recorded shifts
- [[silences]]: keep Beacon quiet about matching alerts for a period, through the API
- [[notifications]]: how notifications are queued, retried and recorded
- [[slack]]: set up the Slack app and send alerts to a channel
