# AGENTS.md

Operational expectations for humans and AI agents working in this repository.

## Purpose

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

The project is at an early stage: alerts are received over a webhook, stored and posted to a Slack channel, where people can acknowledge, snooze and resolve them. Configuration files, users, routing and escalation do not exist yet. `docs/requirements.md` is the source of truth for what Beacon must do, which requirements are in the MVP, and the design decisions already made. Read it before proposing designs or writing code, and update it in the same PR when a decision changes.

## Tech stack

Per `docs/requirements.md`:

- **Go** for the server, shipped as a single binary (`cmd/beacon`). The command line uses the standard library `flag` package; there is no CLI framework.
- **Postgres** as the only required dependency, through [pgx](https://github.com/jackc/pgx) (`pgxpool`). Schema migrations are [goose](https://github.com/pressly/goose) SQL files embedded in the binary.
- **Declarative configuration files** for provisioned objects (sources, integrations, routing, escalation policies, teams, schedule rotations); the database holds runtime state (planned)

- **HTTP**: the standard library `net/http` server and router, `log/slog` for structured logs, and [client_golang](https://github.com/prometheus/client_golang) for `/metrics`. There is no web framework.

Tooling:

- **golangci-lint** for linting and formatting checks, configured in `.golangci.yml`
- **Renovate** for dependency updates (managers: `asdf`, `docker-compose`, `dockerfile`, `github-actions`, `gomod`)
- **GitHub Actions** for the CI, Renovate, release, and docs workflows
- **[nebula-md](https://github.com/thanoslabs/nebula-md)** for building `docs/` into a static markdown site
- **MIT License**

## Structure

```
.
├── .agents/                  # Agent instructions and skills
├── .github/
│   ├── CODEOWNERS            # @thanoslabs/reviewers
│   ├── renovate-config.js    # Renovate platform config
│   └── workflows/
│       ├── ci.yml            # Validation on pushes to main and on pull requests
│       ├── docs.yaml         # nebula-md docs build + GitHub Pages deploy
│       ├── release.yaml      # Label-driven SemVer release workflow
│       └── renovate.yaml     # Renovate workflow
├── cmd/
│   └── beacon/               # Entry point of the single binary
├── docs/                     # Project documentation (markdown source for the docs site)
├── internal/
│   ├── alert/                # Alert model, state machine and timeline
│   ├── cli/                  # Subcommand dispatch and the subcommands themselves
│   ├── config/               # Typed provisioned objects (sources, integrations) and the Secret type
│   ├── ingest/               # Webhook receiver: POST /api/v1/ingest/<source>
│   ├── logging/              # slog setup; request identifiers in log records
│   ├── notify/               # Notification queue, workers, retries and the Channel interface
│   ├── postgres/             # Connection pool and migrator
│   │   ├── migrations/       # Versioned SQL migrations, embedded in the binary
│   │   └── postgrestest/     # Test helper: one empty database per test
│   ├── provision/            # Copies provisioned objects into Postgres
│   ├── server/               # HTTP server, middleware, health, readiness and metrics endpoints
│   ├── slack/                # Slack channel: Web API client, message rendering and button presses
│   │   └── slacktest/        # Fake Slack for tests
│   └── version/              # Build version reporting
├── .dockerignore             # Build context allow-list; add new source directories here
├── .golangci.yml             # golangci-lint configuration
├── .tool-versions            # Pinned language/tool versions (asdf/mise)
├── Dockerfile                # Multi-stage build of the release image
├── compose.yaml              # Beacon and Postgres together; Postgres version pinned
├── AGENTS.md                 # This file
├── CONTRIBUTING.md
├── LICENSE
├── README.md
├── SECURITY.md
├── go.mod                    # Go module: github.com/thanoslabs/beacon
└── renovate.json             # Renovate settings
```

Keep this section and the commands below up to date as code is added.

## Commands

Run `mise install` first so that the pinned versions are used.

| Task | Command |
|---|---|
| Build | `go build -o bin/beacon ./cmd/beacon` |
| Run | `./bin/beacon version` (or `go run ./cmd/beacon version`) |
| Start Postgres | `docker compose up -d --wait postgres` |
| Test | `BEACON_TEST_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' go test -race ./...` |
| Run the server | `BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' ./bin/beacon serve` then `curl localhost:8080/readyz` |
| Apply migrations | `BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' ./bin/beacon migrate` |
| Lint | `golangci-lint run ./...` |
| Fix formatting | `golangci-lint fmt ./...` |
| Check module files | `go mod tidy -diff` |
| Build the image | `docker build -t beacon:dev .` |
| Run Beacon and Postgres | `docker compose up --build -d --wait`, then `curl localhost:8080/readyz` (set `BEACON_PORT` if 8080 is taken) |

CI (`.github/workflows/ci.yml`) runs the build, lint, test and module checks on every pull request, whatever its base branch. It starts Postgres from `compose.yaml`, so CI and development use the same Postgres version. A separate job builds the image, starts Beacon and Postgres with Compose, and checks the endpoints, the non-root user and a clean exit on termination.

### Container image

- The Go version appears in `.tool-versions` and in the `Dockerfile` build stage. Change both together; CI fails when they differ, and Renovate groups them as "go toolchain".
- `.dockerignore` excludes everything except what the `Dockerfile` copies. A new top-level source directory must be added to both files.
- The final stage has no `RUN` step, so building for another platform needs no emulation. Keep it that way, or add QEMU to the release workflow.
- The image runs as the numeric user `65532`. Do not write to the container file system at runtime.

### Server

- `beacon serve` applies pending migrations (unless `--migrate=false`), then serves `GET /healthz`, `GET /readyz` and `GET /metrics` on `--listen-address` (default `:8080`).
- Attach routes to `Server.Mux()` in `internal/server`. Every route passes through the request identifier, access log and panic recovery middleware.
- Log with the `slog` methods that take a context (`InfoContext` and so on) inside request handling, so the record carries `request_id`.
- Register metrics on the registry from `server.NewRegistry()`, not the global default. Keep label values to bounded sets (see the cardinality warning in `docs/requirements.md`).
- The process must stay stateless: anything that has to survive a restart or be seen by another replica goes in Postgres, never in memory or on local disk.

### Database

- Settings are read from flags first, then environment variables (`BEACON_DATABASE_URL`, `BEACON_DATABASE_MAX_CONNS`, `BEACON_DATABASE_CONNECT_TIMEOUT`). Register new settings with the helpers in `internal/cli/flags.go` so both forms always exist.
- Tests that need a database call `postgrestest.New(t)` or `postgrestest.NewMigrated(t)`. Without `BEACON_TEST_DATABASE_URL` they are skipped locally and fail in CI. Always run them against a real Postgres before claiming a database change works; do not mock the database.
- Add a migration as `internal/postgres/migrations/<next 5-digit number>_<snake_case>.sql` with `-- +goose Up` and `-- +goose Down` sections. Never edit a migration that has been released; add a new one. Each migration runs in a transaction, and a Postgres advisory lock keeps replicas from applying one twice.
- The schema follows `docs/configuration-and-data-model.md`. If a migration departs from it, update that page in the same PR.

### Alerts

- `internal/alert` owns the `alerts`, `alert_groups` and `alert_events` tables. Change alert state only through `alert.Store`, so that every change passes the state machine (`alert.Next`) and is written with its timeline event in one transaction. `docs/alert-lifecycle.md` describes the states.
- To react to a state change, register an `alert.Observer`. It runs inside the transaction that makes the change, so what it writes commits or rolls back with it.
- The store takes the time from its clock, not from Postgres, so tests can move time with `SetClock`.
- Background work started by `beacon serve` watches the command's context and is waited for before the command returns. Anything it needs to remember goes in Postgres.

### Configuration and secrets

- Provisioned objects are typed in `internal/config`, following `docs/configuration-and-data-model.md`. Configuration files do not exist yet: `bootstrapSettings` in `internal/cli/bootstrap.go` fills `config.Config` from a few temporary flags and environment variables. Do not add a second route into the features; extend `config.Config`, so the YAML loader can replace the bootstrap settings without touching them.
- Hold every credential in a `config.Secret`. It prints, logs and encodes as a reference to where it came from, never as its value; call `Reveal` only at the point of use. Register a credential setting with `secretSetting`, and add it to `secretSettings` in `internal/cli/secrets_test.go`, which checks that no secret reaches help, errors or logs.
- `provision.Ensure` gives a provisioned object its row, which runtime rows reference.

### Ingestion

- `internal/ingest` serves `POST /api/v1/ingest/<source>`; `docs/webhook.md` is the contract for the generic JSON payload. Every payload error must name the field at fault.
- Receivers change alerts only through `alert.Store`.

### Notifications

- `internal/notify` owns the `notifications` and `notification_attempts` tables. `docs/notifications.md` describes the delivery rules; keep it true.
- Queue a notification with `Pipeline.Enqueue` inside the transaction of the change that causes it, never after the commit and never by sending directly.
- A new notification service is a type that implements `notify.Channel`, registered with `Pipeline.Register`. The pipeline must not learn anything specific to a service. A channel returns `notify.Permanent` for failures that retrying cannot fix and `notify.RetryAfter` when the service says how long to wait; anything else is retried with backoff. Channel errors are stored and shown on the timeline, so they must never contain a credential.
- Delivery is at least once. A channel should make a repeated `Send` harmless where the service allows it.
- `Pipeline.NotifyAll` is temporary: it notifies fixed targets on every state change until routing and escalation exist.

### Slack

- `internal/slack` is the Slack channel. It calls two Web API methods, `chat.postMessage` and `chat.update`, with the standard library; there is no Slack SDK.
- Beacon has no Slack credentials for testing. Test against `slacktest`, the fake Slack, which checks requests against the documented shapes and limits. When you change what is sent, read Slack's current documentation rather than trusting memory, teach the fake the same rule, and say in the PR that real delivery is unverified.
- Everything that comes from an alert passes through `escape` before it goes into message text, and each part is clipped so a message stays within Block Kit's limits.
- Button presses arrive at `POST /api/v1/integrations/<name>/interactions`. The handler verifies the Slack signature over the raw body before it parses anything; keep it that way. It answers within Slack's three seconds, and changes alerts only through `alert.Store.ApplyToGroup` with `alert.ChatUser`, so the timeline records the Slack user in the form that identity linking will use.
- The buttons a message offers are derived from the state machine (`alert.Next`), so they cannot drift from what is allowed.
- `docs/slack.md` is the setup guide. Update it when scopes or settings change.

To add a subcommand, add a file under `internal/cli/` and register it in `commands()` in `internal/cli/cli.go`. Commands take their environment, output streams and arguments as parameters, so tests can run them in-process.

## Conventions

- `.tool-versions` is the single source of truth for language and tool versions. Before building, testing, or running any tooling, check it and use the pinned versions (install via `asdf install` or `mise install`). When adding a new language or tool to the project, pin its version there first — never assume a globally installed version.
- Versioning: project artifacts (releases, tags, packages, images) follow [SemVer](https://semver.org/) as bare `X.Y.Z` — no `v` prefix (`1.4.2`, not `v1.4.2`). Bump MAJOR for breaking changes, MINOR for backwards-compatible features, PATCH for fixes.
- Branch protection: never push directly to `main`; all changes via PR with review.
- Branch names and commit messages follow `CONTRIBUTING.md`.

## Releases

- All released artifacts (images, binaries, packages) are versioned with [SemVer](https://semver.org/) as bare `X.Y.Z` tags — no `v` prefix.
- **Automatic bumps**: when a PR merges to `main`, the next version is derived from the PR label:
  - `major` — breaking changes
  - `minor` — backwards-compatible features
  - `patch` — fixes
  - No label — defaults to a `patch` bump
- **Manual releases**: a specific version may be cut manually by supplying an explicit `X.Y.Z` version tag when needed (e.g. via a manual workflow dispatch). This bypasses the label-based bump.
- Release automation lives in `.github/workflows/release.yaml`: it computes the next version, builds and pushes the container image `ghcr.io/thanoslabs/beacon:X.Y.Z` for `linux/amd64` and `linux/arm64`, then tags and creates the GitHub release. The image is pushed before the tag is created, so a failed build leaves no tag behind. Standalone binaries are not published yet.

## Docs

- Project documentation lives in `docs/` as markdown and is the source for the project's docs site. Do not introduce another static site generator (MkDocs, Docusaurus, etc.) — docs are built with [nebula-md](https://github.com/thanoslabs/nebula-md).
- `.github/workflows/docs.yaml` calls nebula-md's reusable workflow (`thanoslabs/nebula-md/.github/workflows/deploy.yml`) on pushes to `main` that touch `docs/`. Generation happens in CI only — do not commit built HTML.
- Deployment to GitHub Pages runs only when the repository is **public**; private repos skip the job. Public repos must set **Settings → Pages → Source** to **GitHub Actions**.
- Pages are Obsidian-flavoured markdown: YAML frontmatter (`title`, `tags`, `date`), `[[wikilinks]]` between pages, and callouts. `docs/index.md` is the site's landing page.
