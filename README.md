# Beacon

[![CI](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/thanoslabs/beacon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/thanoslabs/beacon?sort=semver&display_name=tag)](https://github.com/thanoslabs/beacon/releases/latest)
[![Docs](https://github.com/thanoslabs/beacon/actions/workflows/docs.yaml/badge.svg?branch=main)](https://thanoslabs.github.io/beacon/)
[![Renovate](https://img.shields.io/badge/renovate-enabled-brightgreen?logo=renovatebot)](https://github.com/thanoslabs/beacon/pulls?q=is%3Apr+author%3Aapp%2Frenovate)
[![SemVer](https://img.shields.io/badge/semver-2.0.0-blue)](https://semver.org/)
[![License: MIT](https://img.shields.io/github/license/thanoslabs/beacon)](LICENSE)

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

> [!WARNING]
> Beacon is at an early stage. The `beacon` command builds and runs, but it does not handle alerts yet. The [requirements](docs/requirements.md) describe what is planned.

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

Beacon needs Postgres. `compose.yaml` starts the pinned version for development, with throwaway credentials:

```sh
docker compose up -d --wait postgres
export BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
./bin/beacon migrate            # apply schema migrations
./bin/beacon migrate --status   # list migrations and their state
```

Tests that need a database are skipped unless `BEACON_TEST_DATABASE_URL` points at a Postgres server. Each test creates and drops its own database, so the server's other contents are left alone:

```sh
export BEACON_TEST_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
go test -race ./...
```

Every setting can be given as a flag or as an environment variable; run `beacon <command> -h` to list them.

- **Releases**: merging a PR to `main` cuts a [SemVer](https://semver.org/) release with a bare `X.Y.Z` tag. The bump comes from the PR's `major`, `minor` or `patch` label, and defaults to `patch`.
- **Dependencies**: [Renovate](https://docs.renovatebot.com/) opens update PRs, configured in `renovate.json`.

See [CONTRIBUTING.md](CONTRIBUTING.md) for branch naming, commit style and the pull request process, and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

[MIT](LICENSE)
