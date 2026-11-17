---
title: Home
---

Project documentation. Every markdown file under `docs/` is built into a static site by [nebula-md](https://github.com/thanoslabs/nebula-md) and published to GitHub Pages on merge to `main` (public repositories only).

Link between pages with Obsidian-style `[[wikilinks]]`.

## Pages

- [[requirements]]: what Beacon must do, split into MVP and later
- [[configuration]]: reference for the YAML configuration files, and how to check them
- [[configuration-and-data-model]]: the design behind the configuration schema and the Postgres data model
- [[alert-lifecycle]]: the states an alert moves through, and its timeline
- [[webhook]]: send alerts to Beacon as JSON over HTTP
- [[notifications]]: how notifications are queued, retried and recorded
- [[slack]]: set up the Slack app and send alerts to a channel
