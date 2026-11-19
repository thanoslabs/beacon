# Beacon

[![CI](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/thanoslabs/beacon?sort=semver&display_name=tag)](https://github.com/thanoslabs/beacon/releases/latest)
[![Docs](https://github.com/thanoslabs/beacon/actions/workflows/docs.yaml/badge.svg?branch=main)](https://thanoslabs.github.io/beacon/)
[![Renovate](https://img.shields.io/badge/renovate-enabled-brightgreen?logo=renovatebot)](https://github.com/thanoslabs/beacon/pulls?q=is%3Apr+author%3Aapp%2Frenovate)
[![SemVer](https://img.shields.io/badge/semver-2.0.0-blue)](https://semver.org/)
[![License: MIT](https://img.shields.io/github/license/thanoslabs/beacon)](LICENSE)

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

> [!WARNING]
> Beacon is at an early stage. It reads its [configuration](docs/configuration.md) from YAML files, signs people in through an [identity provider](docs/access.md), receives alerts from [Alertmanager](docs/alertmanager.md), [Grafana alerting](docs/grafana.md) and through a [JSON webhook](docs/webhook.md), tracks their [state](docs/alert-lifecycle.md), [routes](docs/configuration.md#how-an-alert-is-routed) them by label, [groups](docs/configuration.md#grouping) related alerts and [escalates](docs/escalation.md) each group through its policy: it pages the person on call, the team or a [Slack](docs/slack.md) or [Discord](docs/discord.md) channel, in chat and by [email](docs/email.md), step by step, until someone acknowledges, snoozes or resolves the alert from Slack or Discord, and it [attributes](docs/access.md#chat-accounts) those actions to the Beacon user behind the chat account. It runs [on-call rotations](docs/schedules.md), records every shift, takes [overrides](docs/schedules.md#overrides), answers [who is on call](docs/schedules.md#who-is-on-call) and [previews](docs/schedules.md#preview-a-change) what a change to a schedule would move. Outbound webhooks do not exist yet, neither chat integration has been tried against the real service, and there is no web interface yet. The [requirements](docs/requirements.md) describe what is planned.

## What Beacon will do

The first release is planned to cover:

- **Ingestion**: receive alerts from Alertmanager, Grafana alerting and a generic JSON webhook.
- **Routing**: deduplicate and group alerts, then route them by label to a team or service.
- **On-call schedules**: rotations and overrides, with a view of who is on call now.
- **Escalation**: multi-step policies that repeat until someone acknowledges.
- **Notifications**: Slack, Discord, email and outbound webhooks, with acknowledge and resolve from chat.
- **Metrics**: a Prometheus `/metrics` endpoint covering both Beacon itself and how alerts are handled.

The full list, including what comes later, is in the [requirements](docs/requirements.md).

## Design principles

- **Five minutes to the first page.** A new user gets from `docker compose up` to a test page in Slack or Discord in about five minutes.
- **Configuration as code first.** Sources, integrations, routing and escalation policies are defined in files that live in version control. The database holds runtime state such as alert status and schedule overrides.
- **One binary plus Postgres.** Beacon ships as a single Go binary and needs only a Postgres database.
- **Works with what people already run.** Beacon sits behind an existing Alertmanager or Grafana setup.

## Run it

Beacon needs only Postgres. With Docker, this builds Beacon from the checkout and starts both:

```sh
docker compose up --build
curl http://localhost:8080/readyz
```

Set `BEACON_PORT` to publish Beacon on a different host port, for example `BEACON_PORT=18080 docker compose up --build`.

Compose gives Beacon the configuration in [`examples/quickstart`](examples/quickstart/beacon.yaml): one webhook source named `default`. Set its token before starting, then send an alert:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
docker compose up --build -d --wait
curl -X POST http://localhost:8080/api/v1/ingest/default \
  -H "Authorization: Bearer $BEACON_SOURCE_TOKEN" \
  -d '{"labels": {"alertname": "DiskFull", "severity": "critical"}, "annotations": {"summary": "Disk is full"}}'
```

Without `BEACON_SOURCE_TOKEN`, Compose uses a fixed token that is only fit for a local try-out.

The [webhook page](docs/webhook.md) describes the payload and the responses. Sources, integrations and everything else that is provisioned are defined in YAML files; the [configuration page](docs/configuration.md) is the reference. To use your own, point Compose at a directory of them with `BEACON_CONFIG_DIR`. To have alerts posted to Slack, start from [`examples/slack`](examples/slack/beacon.yaml) as the [Slack page](docs/slack.md) describes; for Discord, from [`examples/discord`](examples/discord/beacon.yaml) and the [Discord page](docs/discord.md); for email, from [`examples/email`](examples/email/beacon.yaml) and the [email page](docs/email.md). Alerts can be resolved from Slack straight away; acknowledging and snoozing from Slack need people to sign in to Beacon through an identity provider first, so that Beacon knows whose Slack account is whose ([who can act](docs/slack.md#who-can-act)). For a rotation and a policy that pages whoever is on call, see [`examples/on-call`](examples/on-call/beacon.yaml). To put Beacon behind an Alertmanager or Grafana alerting, see the [Alertmanager page](docs/alertmanager.md) and the [Grafana page](docs/grafana.md); `docker compose --profile alertmanager up` and `--profile grafana` start one that sends to Beacon.

Releases publish a container image for `linux/amd64` and `linux/arm64`, tagged with the bare version:

```sh
docker run --rm -p 8080:8080 \
  -e BEACON_DATABASE_URL='postgres://user:password@host:5432/beacon' \
  -e BEACON_CONFIG=/etc/beacon -v "$PWD/my-config:/etc/beacon:ro" \
  ghcr.io/thanoslabs/beacon:X.Y.Z
```

The image runs as a non-root user (`65532`) and its default command is `beacon serve`.

## Documentation

Documentation lives in [`docs/`](docs/) and is published to <https://thanoslabs.github.io/beacon/> on every merge to `main` that touches it. The site is built with [nebula-md](https://github.com/thanoslabs/nebula-md).

## Development

`.tool-versions` pins Go and the other tools. Install them with `mise install` (or `asdf install`) before running anything.

```sh
go build -o bin/beacon ./cmd/beacon   # build the binary
./bin/beacon version                  # run it
go test -race ./...                   # run the tests
golangci-lint run ./...               # lint
```

To run the binary outside Docker, start only Postgres from `compose.yaml`, which pins the version and uses throwaway credentials:

```sh
docker compose up -d --wait postgres
export BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
./bin/beacon serve --config examples/quickstart   # apply pending migrations, then serve on :8080
./bin/beacon migrate            # apply schema migrations without serving
./bin/beacon migrate --status   # list migrations and their state
./bin/beacon validate --config examples/full   # check configuration files; needs no database
./bin/beacon preview --config examples/full    # show which on-call shifts a configuration would move; reads the database
./bin/beacon hash-password      # hash a password for the bootstrap admin
```

The server answers on these endpoints. Everything that is not listed as open in the [access page](docs/access.md) needs a session, and an address that does not exist answers `401` like one that does:

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/ingest/<source>` | Receives alerts from a source: Alertmanager's notifications ([Alertmanager page](docs/alertmanager.md)), Grafana's ([Grafana page](docs/grafana.md)) or the generic JSON payload ([webhook page](docs/webhook.md)), by the type of the source. |
| `POST /api/v1/integrations/<name>/interactions` | Receives button presses from Slack, and button presses and the link command from Discord, by the type of the integration. See the [Slack page](docs/slack.md) and the [Discord page](docs/discord.md). |
| `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` | Sign-in through an OpenID Connect identity provider. See the [access page](docs/access.md). |
| `GET /api/v1/me` | Who is signed in. Needs a session. |
| `GET /api/v1/me/identities`, `POST /api/v1/me/identities/<integration>/link-code`, `DELETE /api/v1/me/identities/<integration>` | The chat accounts linked to whoever is signed in, a one-time code with which they link their Discord account, and its removal. See the [access page](docs/access.md#chat-accounts). |
| `POST /api/v1/config/reload` | Reloads the configuration. Needs the admin role. |
| `GET`, `POST /api/v1/tokens`, `DELETE /api/v1/tokens/<id>` | API tokens for automation. Needs the admin role. |
| `GET`, `POST /api/v1/silences`, `GET`, `DELETE /api/v1/silences/<id>` | Silences. Needs a session or a token, with either role. See the [silences page](docs/silences.md). |
| `GET /api/v1/schedules`, `GET /api/v1/schedules/<name>`, `GET /api/v1/schedules/<name>/shifts`, `GET /api/v1/teams/<name>/on-call` | Who is on call, now and over a period, for a schedule and for a team. Needs a session or a token, with either role. See the [schedules page](docs/schedules.md#who-is-on-call). |
| `GET`, `POST /api/v1/schedules/<name>/overrides`, `DELETE /api/v1/schedules/<name>/overrides/<id>` | Overrides: someone covers a schedule for a period. Needs a session or a token, with either role. See the [schedules page](docs/schedules.md#overrides). |
| `POST /auth/bootstrap` | Sign-in of the bootstrap admin, for installs with no identity provider. Off unless configured. |
| `GET /healthz` | Liveness: the process is running. It does not check the database. |
| `GET /readyz` | Readiness: the database answers and the server is not shutting down. Returns `503` otherwise. |
| `GET /metrics` | Prometheus metrics: Go runtime, process and build information, configuration reloads, team and rotation members that are not users yet, users without a linked chat account, and alerts that no routing rule matched. |

Logs are JSON on standard error (`--log-format text` for reading by eye). Every request gets an identifier, returned in the `X-Request-Id` header and written on each log record for that request. On `SIGTERM` or `SIGINT` the server stops accepting connections and lets in-flight requests finish, for up to `--shutdown-timeout`. On `SIGHUP` it [reloads its configuration](docs/configuration.md#reload-without-a-restart).

Tests that need a database are skipped unless `BEACON_TEST_DATABASE_URL` points at a Postgres server. Each test creates and drops its own database, so the server's other contents are left alone:

```sh
export BEACON_TEST_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
go test -race ./...
```

The sign-in tests need an identity provider. `compose.yaml` has one, [Dex](https://dexidp.io/), which is started by name and is skipped by the tests unless its address is set:

```sh
docker compose up -d --wait postgres dex
export BEACON_TEST_OIDC_ISSUER='http://127.0.0.1:5556/dex'
```

The tests of the email channel need an SMTP server. `compose.yaml` has one, [Mailpit](https://mailpit.axllent.org/), which keeps every message and delivers none; it is started by name and is skipped by the tests unless its addresses are set:

```sh
docker compose up -d --wait mailpit
export BEACON_TEST_SMTP_ADDR='127.0.0.1:1025' BEACON_TEST_MAILPIT_URL='http://127.0.0.1:8025'
```

The tests in `internal/e2e` build the binary and run it as a process against Postgres, a fake Slack, a fake Discord and the SMTP server above: an alert goes in by webhook, a message comes out to Slack, a button press acknowledges it and the source resolves it; and the same from Discord, where a person first links their account with a code. They run with the rest of the tests.

Every process setting can be given as a flag or as an environment variable; run `beacon <command> -h` to list them. Provisioned objects come from the files named by `--config` (or `BEACON_CONFIG`); without it Beacon starts with no sources and notifies nobody.

- **Releases**: merging a PR to `main` cuts a [SemVer](https://semver.org/) release with a bare `X.Y.Z` tag and publishes the container image `ghcr.io/thanoslabs/beacon:X.Y.Z`. The bump comes from the PR's `major`, `minor` or `patch` label, and defaults to `patch`.
- **Dependencies**: [Renovate](https://docs.renovatebot.com/) opens update PRs, configured in `renovate.json`.

See [CONTRIBUTING.md](CONTRIBUTING.md) for branch naming, commit style and the pull request process, and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

[MIT](LICENSE)
