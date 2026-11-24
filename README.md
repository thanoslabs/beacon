# Beacon

[![CI](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/thanoslabs/beacon?sort=semver&display_name=tag)](https://github.com/thanoslabs/beacon/releases/latest)
[![Docs](https://github.com/thanoslabs/beacon/actions/workflows/docs.yaml/badge.svg?branch=main)](https://thanoslabs.github.io/beacon/)
[![Renovate](https://img.shields.io/badge/renovate-enabled-brightgreen?logo=renovatebot)](https://github.com/thanoslabs/beacon/pulls?q=is%3Apr+author%3Aapp%2Frenovate)
[![SemVer](https://img.shields.io/badge/semver-2.0.0-blue)](https://semver.org/)
[![License: MIT](https://img.shields.io/github/license/thanoslabs/beacon)](LICENSE)

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

It receives alerts from [Alertmanager](docs/alertmanager.md), [Grafana alerting](docs/grafana.md) or a [JSON webhook](docs/webhook.md), [routes and groups](docs/configuration.md#how-an-alert-is-routed) them by label, and [escalates](docs/escalation.md) each group through a policy: the person [on call](docs/schedules.md), then the team, then a channel, in [Slack](docs/slack.md), [Discord](docs/discord.md), [email](docs/email.md) or an [outbound webhook](docs/outbound-webhook.md), until someone acknowledges. People respond from chat or from the [web interface](docs/web.md), which is in the binary.

## Quickstart

With Docker, from a clone of this repository:

```sh
docker compose up --build -d --wait             # Beacon and Postgres
docker compose exec beacon beacon test-alert    # send a test alert
```

Open <http://localhost:8080>, sign in with the password `local-development-password`, and the alert is there. The **[quickstart guide](docs/quickstart.md)** takes it from here to a page in Slack or Discord, in about five minutes, and says what a real install needs beyond that. The password and the token of this first run are published in `compose.yaml` and are for your own machine only.

## What Beacon does

- **Ingestion**: alerts from Alertmanager, Grafana alerting and a generic JSON webhook, deduplicated by fingerprint and resolved when the source resolves them.
- **Routing**: label-based rules that give an alert its team, service and escalation policy, with related alerts grouped into one notification, and [silences](docs/silences.md).
- **On-call schedules**: rotations with handoffs in the schedule's own time zone, layered overrides, a record of every shift, and a [preview](docs/schedules.md#preview-a-change) of what a change would move.
- **Escalation**: multi-step policies with timeouts that page the person on call, teams and channels, and repeat until someone responds.
- **Notifications**: Slack, Discord, email and signed outbound webhooks, with retries and a timeline of every attempt. Acknowledge, snooze and resolve from Slack and Discord.
- **A web interface and an API**: alerts, who is on call, overrides, silences, and what the configuration defines; sign-in through any OpenID Connect provider, and [API tokens](docs/access.md#api-tokens). The API has an [OpenAPI description](docs/api-reference.md).
- **Metrics**: a Prometheus [`/metrics`](docs/metrics.md) endpoint for how Beacon runs and how alerts are handled, with alerting rules and a Grafana dashboard to copy.
- **Configuration as code**: sources, routing, policies, teams and schedules are [YAML files](docs/configuration.md), checked by `beacon validate` and reloaded without a restart.

What is planned and not built, such as SMS, voice calls, heartbeat checks and a Helm chart, is listed in the [quickstart guide](docs/quickstart.md#what-is-not-in-this-release) and in the [requirements](docs/requirements.md).

## Design principles

- **Five minutes to the first page.** A new user gets from `docker compose up` to a test page in Slack or Discord in about five minutes.
- **Configuration as code first.** Sources, integrations, routing and escalation policies are defined in files that live in version control. The database holds runtime state such as alert status and schedule overrides.
- **One binary plus Postgres.** Beacon ships as a single Go binary and needs only a Postgres database.
- **Works with what people already run.** Beacon sits behind an existing Alertmanager or Grafana setup.
- **Observable by default.** Beacon reports on itself as Prometheus metrics.

## Run it

Releases publish a container image for `linux/amd64` and `linux/arm64`, tagged with the bare version. It needs a Postgres database and a directory of configuration files:

```sh
docker run --rm -p 8080:8080 \
  -e BEACON_DATABASE_URL='postgres://user:password@host:5432/beacon' \
  -e BEACON_CONFIG=/etc/beacon -v "$PWD/my-config:/etc/beacon:ro" \
  ghcr.io/thanoslabs/beacon:X.Y.Z
```

The image runs as a non-root user (`65532`) and its default command is `beacon serve`. Beacon applies its database migrations when it starts.

[`examples/`](examples/) has a configuration to start from for each thing Beacon does: [`quickstart`](examples/quickstart/beacon.yaml), [`slack`](examples/slack/beacon.yaml), [`discord`](examples/discord/beacon.yaml), [`email`](examples/email/beacon.yaml), [`outbound-webhook`](examples/outbound-webhook/beacon.yaml), [`alertmanager`](examples/alertmanager/beacon.yaml), [`grafana`](examples/grafana/beacon.yaml), [`on-call`](examples/on-call/beacon.yaml) for a rotation and a policy that pages whoever is on call, and [`full`](examples/full/beacon.yaml) for every field. The [configuration page](docs/configuration.md) is the reference. With the Compose file of this repository, `BEACON_CONFIG_DIR=./my-config docker compose up` runs a directory of your own, `BEACON_PORT` moves Beacon off port 8080, and `--profile alertmanager` or `--profile grafana` adds one of those, set up to send to Beacon.

Acknowledging and snoozing from chat need people to sign in to Beacon through an identity provider, so that Beacon knows whose chat account is whose ([who can act](docs/slack.md#who-can-act)); anyone who sees a message may resolve it.

> [!NOTE]
> Beacon is young. Slack, Discord, hosted identity providers and hosted mail services are tested against faithful stand-ins and against Dex and Mailpit, not yet against the real services. Tell us what you find.

## Documentation

Documentation lives in [`docs/`](docs/) and is published to <https://thanoslabs.github.io/beacon/> on every merge to `main` that touches it. The site is built with [nebula-md](https://github.com/thanoslabs/nebula-md).

## Development

`.tool-versions` pins Go, Node and the other tools. Install them with `mise install` (or `asdf install`) before running anything.

The web interface is in [`web/`](web/) and is embedded in the binary from where its build is written. Build it before the binary to have it in the binary; Go alone builds and tests without it, and without Node. The [web interface page](docs/web.md#development) describes the development server, the generated API types and the browser tests.

```sh
(cd web && npm ci && npm run build)   # build the web interface; optional for work on Go alone
go build -o bin/beacon ./cmd/beacon   # build the binary
./bin/beacon version                  # run it
go test -race ./...                   # run the tests
golangci-lint run ./...               # lint
```

To run the binary outside Docker, start only Postgres from `compose.yaml`, which pins the version and uses throwaway credentials:

```sh
docker compose up -d --wait postgres
export BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
# The published credentials of the quickstart: the admin's password is local-development-password.
export BEACON_SOURCE_TOKEN=local-development-token BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH='$2a$12$AxLH.cSYxO4J7BQSRKiZceiSKU7VLV8.WPzpFtwpkCgeMoYxt8AiW'
./bin/beacon serve --config examples/quickstart   # apply pending migrations, then serve on :8080
./bin/beacon migrate            # apply schema migrations without serving
./bin/beacon migrate --status   # list migrations and their state
./bin/beacon validate --config examples/full   # check configuration files; needs no database
./bin/beacon preview --config examples/full    # show which on-call shifts a configuration would move; reads the database
./bin/beacon hash-password      # hash a password for the bootstrap admin
./bin/beacon test-alert         # send a test alert to a running Beacon; reads BEACON_SOURCE_TOKEN
```

The server answers on these endpoints; the [API page](docs/api.md) describes how they behave as a whole, and the [API reference](docs/api-reference.md) lists every route from the OpenAPI description. Everything that is not listed as open in the [access page](docs/access.md) needs a session, and an address under `/api/` or `/auth/` that does not exist answers `401` like one that does:

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/ingest/<source>` | Receives alerts from a source: Alertmanager's notifications ([Alertmanager page](docs/alertmanager.md)), Grafana's ([Grafana page](docs/grafana.md)) or the generic JSON payload ([webhook page](docs/webhook.md)), by the type of the source. |
| `POST /api/v1/integrations/<name>/interactions` | Receives button presses from Slack, and button presses and the link command from Discord, by the type of the integration. See the [Slack page](docs/slack.md) and the [Discord page](docs/discord.md). |
| `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` | Sign-in through an OpenID Connect identity provider. See the [access page](docs/access.md). |
| `GET /api/v1/me` | Who is signed in. Needs a session. |
| `GET /api/v1/me/identities`, `POST /api/v1/me/identities/<integration>/link-code`, `DELETE /api/v1/me/identities/<integration>` | The chat accounts linked to whoever is signed in, a one-time code with which they link their Discord account, and its removal. See the [access page](docs/access.md#chat-accounts). |
| `GET /api/v1/alerts`, `GET /api/v1/alerts/<id>`, `GET /api/v1/alerts/<id>/timeline`, `POST /api/v1/alerts/<id>/acknowledge`, `.../snooze`, `.../resolve`, and the same under `/api/v1/alert-groups/<id>` | Alerts: list with filters, read, and act on one alert or on a whole group. Needs a session or a token, with either role. See the [API page](docs/api.md#alerts). |
| `GET /api/v1/sources`, `/api/v1/integrations`, `/api/v1/escalation-policies`, `/api/v1/routing`, `/api/v1/teams`, each also by `/<name>` except routing, and `GET /api/v1/users` | What the configuration defines, for reading, without credentials, and the users. Needs a session or a token, with either role. See the [API page](docs/api.md#what-the-configuration-defines). |
| `GET /auth/methods` | Which ways to sign in exist. Open. |
| `GET /api/v1/openapi.json`, `GET /api/v1/openapi.yaml` | The [OpenAPI description](docs/api.md#openapi-description) of the API. Open. |
| `POST /api/v1/config/reload` | Reloads the configuration. Needs the admin role. |
| `GET`, `POST /api/v1/tokens`, `DELETE /api/v1/tokens/<id>` | API tokens for automation. Needs the admin role. |
| `GET`, `POST /api/v1/silences`, `POST /api/v1/silences/preview`, `GET`, `DELETE /api/v1/silences/<id>` | Silences, and which open alerts one would match. Needs a session or a token, with either role. See the [silences page](docs/silences.md). |
| `GET /api/v1/schedules`, `GET /api/v1/schedules/<name>`, `GET /api/v1/schedules/<name>/shifts`, `GET /api/v1/teams/<name>/on-call` | Who is on call, now and over a period, for a schedule and for a team. Needs a session or a token, with either role. See the [schedules page](docs/schedules.md#who-is-on-call). |
| `GET`, `POST /api/v1/schedules/<name>/overrides`, `DELETE /api/v1/schedules/<name>/overrides/<id>` | Overrides: someone covers a schedule for a period. Needs a session or a token, with either role. See the [schedules page](docs/schedules.md#overrides). |
| `POST /auth/bootstrap` | Sign-in of the bootstrap admin, for installs with no identity provider. Off unless configured. |
| `GET` on every other address outside `/api/` and `/auth/` | The [web interface](docs/web.md). Open: it is the same page for everyone, and shows only the sign-in page without a session. |
| `GET /healthz` | Liveness: the process is running. It does not check the database. |
| `GET /readyz` | Readiness: the database answers and the server is not shutting down. Returns `503` otherwise. |
| `GET /metrics` | Prometheus [metrics](docs/metrics.md) of how Beacon runs and how alerts are handled. Open unless a metrics token is set. |

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
