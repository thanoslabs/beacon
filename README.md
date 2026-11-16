# Beacon

[![CI](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/thanoslabs/beacon?sort=semver&display_name=tag)](https://github.com/thanoslabs/beacon/releases/latest)
[![Docs](https://github.com/thanoslabs/beacon/actions/workflows/docs.yaml/badge.svg?branch=main)](https://thanoslabs.github.io/beacon/)
[![Renovate](https://img.shields.io/badge/renovate-enabled-brightgreen?logo=renovatebot)](https://github.com/thanoslabs/beacon/pulls?q=is%3Apr+author%3Aapp%2Frenovate)
[![SemVer](https://img.shields.io/badge/semver-2.0.0-blue)](https://semver.org/)
[![License: MIT](https://img.shields.io/github/license/thanoslabs/beacon)](LICENSE)

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

> [!WARNING]
> Beacon is at an early stage. It receives alerts through a [JSON webhook](docs/webhook.md), tracks their [state](docs/alert-lifecycle.md) and posts them to one [Slack channel](docs/slack.md). Routing, schedules and escalation do not exist yet, and the Slack integration has not been tried against a real workspace. The [requirements](docs/requirements.md) describe what is planned.

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

To receive alerts, give the webhook source a token before starting, then send an alert:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
docker compose up --build -d --wait
curl -X POST http://localhost:8080/api/v1/ingest/default \
  -H "Authorization: Bearer $BEACON_SOURCE_TOKEN" \
  -d '{"labels": {"alertname": "DiskFull", "severity": "critical"}, "annotations": {"summary": "Disk is full"}}'
```

The [webhook page](docs/webhook.md) describes the payload and the responses. To have alerts posted to Slack, also set `BEACON_SLACK_BOT_TOKEN` and `BEACON_SLACK_CHANNEL` as the [Slack page](docs/slack.md) describes.

Releases publish a container image for `linux/amd64` and `linux/arm64`, tagged with the bare version:

```sh
docker run --rm -p 8080:8080 \
  -e BEACON_DATABASE_URL='postgres://user:password@host:5432/beacon' \
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
./bin/beacon serve              # apply pending migrations, then serve on :8080
./bin/beacon migrate            # apply schema migrations without serving
./bin/beacon migrate --status   # list migrations and their state
```

The server answers on these endpoints:

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/ingest/<source>` | Receives alerts from a source. See the [webhook page](docs/webhook.md). |
| `GET /healthz` | Liveness: the process is running. It does not check the database. |
| `GET /readyz` | Readiness: the database answers and the server is not shutting down. Returns `503` otherwise. |
| `GET /metrics` | Prometheus metrics: Go runtime, process and build information. |

Logs are JSON on standard error (`--log-format text` for reading by eye). Every request gets an identifier, returned in the `X-Request-Id` header and written on each log record for that request. On `SIGTERM` or `SIGINT` the server stops accepting connections and lets in-flight requests finish, for up to `--shutdown-timeout`.

Tests that need a database are skipped unless `BEACON_TEST_DATABASE_URL` points at a Postgres server. Each test creates and drops its own database, so the server's other contents are left alone:

```sh
export BEACON_TEST_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
go test -race ./...
```

Every setting can be given as a flag or as an environment variable; run `beacon <command> -h` to list them.

- **Releases**: merging a PR to `main` cuts a [SemVer](https://semver.org/) release with a bare `X.Y.Z` tag and publishes the container image `ghcr.io/thanoslabs/beacon:X.Y.Z`. The bump comes from the PR's `major`, `minor` or `patch` label, and defaults to `patch`.
- **Dependencies**: [Renovate](https://docs.renovatebot.com/) opens update PRs, configured in `renovate.json`.

See [CONTRIBUTING.md](CONTRIBUTING.md) for branch naming, commit style and the pull request process, and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

[MIT](LICENSE)
