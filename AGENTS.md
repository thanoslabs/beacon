# AGENTS.md

Operational expectations for humans and AI agents working in this repository.

## Purpose

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

The project is at an early stage: provisioned objects are read from YAML files, people sign in through an OpenID Connect identity provider, alerts are received from Alertmanager, Grafana alerting or over a generic webhook, stored and posted to a Slack channel, where people can acknowledge, snooze and resolve them. Routing, schedules, escalation and the web interface do not exist yet. `docs/requirements.md` is the source of truth for what Beacon must do, which requirements are in the MVP, and the design decisions already made. Read it before proposing designs or writing code, and update it in the same PR when a decision changes.

## Tech stack

Per `docs/requirements.md`:

- **Go** for the server, shipped as a single binary (`cmd/beacon`). The command line uses the standard library `flag` package; there is no CLI framework.
- **Postgres** as the only required dependency, through [pgx](https://github.com/jackc/pgx) (`pgxpool`). Schema migrations are [goose](https://github.com/pressly/goose) SQL files embedded in the binary.
- **Declarative configuration files** in YAML for provisioned objects (sources, integrations, routing, escalation policies, teams, schedule rotations), read with [go.yaml.in/yaml/v3](https://github.com/yaml/go-yaml); the database holds runtime state

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
├── dev/
│   ├── alertmanager/         # Configuration of the Alertmanager of the Compose profile "alertmanager"
│   ├── grafana/              # Provisioning of the Grafana of the Compose profile "grafana", and a rule to try it with
│   └── dex/                  # Configuration of the identity provider used in development and tests
├── docs/                     # Project documentation (markdown source for the docs site)
├── examples/                 # Example configurations; a test keeps them valid
├── internal/
│   ├── access/               # Roles and the principal behind a request
│   ├── alert/                # Alert model, state machine and timeline
│   ├── auth/                 # Sign-in through OpenID Connect, users and sessions
│   │   └── oidctest/         # Test helper: sign in through the Dex of compose.yaml
│   ├── cli/                  # Subcommand dispatch and the subcommands themselves
│   ├── config/               # Typed provisioned objects, the YAML loader and the Secret type
│   ├── e2e/                  # End-to-end tests: the real binary, Postgres and a fake Slack
│   ├── ingest/               # Receivers for Alertmanager, Grafana and the generic webhook: POST /api/v1/ingest/<source>
│   ├── logging/              # slog setup; request identifiers in log records
│   ├── notify/               # Notification queue, workers, retries and the Channel interface
│   ├── postgres/             # Connection pool and migrator
│   │   ├── migrations/       # Versioned SQL migrations, embedded in the binary
│   │   └── postgrestest/     # Test helper: one empty database per test
│   ├── provision/            # Reconciles provisioned objects into Postgres
│   │   └── provisiontest/    # Test helper: a provisioned row to reference
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
| Start Postgres and the identity provider | `docker compose up -d --wait postgres dex` |
| Test | `BEACON_TEST_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' BEACON_TEST_OIDC_ISSUER='http://127.0.0.1:5556/dex' go test -race ./...` |
| Run the server | `BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' BEACON_SOURCE_TOKEN=local-development-token ./bin/beacon serve --config examples/quickstart` then `curl localhost:8080/readyz` |
| Check configuration | `./bin/beacon validate --config examples/full` |
| Hash a password for the bootstrap admin | `./bin/beacon hash-password` |
| Apply migrations | `BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' ./bin/beacon migrate` |
| End-to-end tests only | `BEACON_TEST_DATABASE_URL=... go test -race -count=1 ./internal/e2e/` |
| Lint | `golangci-lint run ./...` |
| Fix formatting | `golangci-lint fmt ./...` |
| Check module files | `go mod tidy -diff` |
| Build the image | `docker build -t beacon:dev .` |
| Run Beacon and Postgres | `docker compose up --build -d --wait`, then `curl localhost:8080/readyz` (set `BEACON_PORT` if 8080 is taken) |
| Run them with an Alertmanager that sends to Beacon | `BEACON_CONFIG_DIR=./examples/alertmanager docker compose --profile alertmanager up --build -d --wait` (set `BEACON_ALERTMANAGER_PORT` if 9093 is taken) |
| Run them with a Grafana that sends to Beacon | `BEACON_CONFIG_DIR=./examples/grafana docker compose --profile grafana up --build -d --wait` (set `BEACON_GRAFANA_PORT` if 3000 is taken), then create the rule as `docs/grafana.md` shows |

CI (`.github/workflows/ci.yml`) runs the build, lint, test and module checks on every pull request, whatever its base branch. It starts Postgres and Dex from `compose.yaml`, so CI and development use the same versions. The tests include the end-to-end tests in `internal/e2e`. A separate job builds the image, starts Beacon and Postgres with Compose and the configuration in `examples/quickstart`, and checks the endpoints, that the webhook accepts an alert, the non-root user and a clean exit on termination. It then starts the Alertmanager and the Grafana of the Compose profiles `alertmanager` and `grafana` and checks that an alert of each fires and resolves in Beacon.

### Container image

- The Go version appears in `.tool-versions` and in the `Dockerfile` build stage. Change both together; CI fails when they differ, and Renovate groups them as "go toolchain".
- `.dockerignore` excludes everything except what the `Dockerfile` copies. A new top-level source directory must be added to both files.
- The final stage has no `RUN` step, so building for another platform needs no emulation. Keep it that way, or add QEMU to the release workflow.
- The image runs as the numeric user `65532`. Do not write to the container file system at runtime.

### Server

- `beacon serve` reads and validates the configuration, applies pending migrations (unless `--migrate=false`), then serves `GET /healthz`, `GET /readyz` and `GET /metrics` on `--listen-address` (default `:8080`).
- Attach a route with `Server.Handle(access.Route{Pattern, Level, Handler})`; there is no other way to the router. Every route passes through the request identifier, access log and panic recovery middleware, and every route that is not `access.Public` through the guard in `internal/access`: `401` without a valid credential, `403` for a cross-site request that changes something, `403` for a member on an `access.Admin` route. The handler finds who is asking with `access.FromContext`.
- **Beacon is closed by default.** A new route is `access.Member` or `access.Admin`. The zero value of the level is `Admin`, so a forgotten level is closed. `access.Public` is only for a route that a machine without a session must reach, and then the handler must check a credential of its own (the source's token, Slack's signature) unless the route is harmless to a stranger. Every route of `beacon serve` is listed in `provisioner.routes` in `internal/cli/app.go` and, on purpose, a second time in `wantRoutes` in `internal/cli/routes_test.go`: adding a route means writing its level down twice and justifying a public one in the comment there.
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

- Provisioned objects are typed in `internal/config`, following `docs/configuration-and-data-model.md`. `config.Load` reads them from one YAML file or a directory, and `beacon serve` takes that path as `--config`. The files are the only route into the features: do not add a flag or an environment variable for something that is provisioned. Process settings, such as the listen address, stay flags.
- Everything that follows the configuration is applied by the `provisioner` in `internal/cli/app.go`, at start and on every reload (`SIGHUP`). It reconciles first and swaps afterwards, so a failure leaves the previous configuration active. A feature that reads provisioned objects must take them through a setter that is safe while requests run (`ingest.Handler.SetSources`, `notify.Pipeline.SetChannels`), called from the provisioner; never capture configuration at start. `TestReload` in `internal/e2e` is the guard: extend it when a feature starts to follow the configuration.
- A kind or type can be valid in the files before it does anything. `beacon serve` stores it and logs a warning that names it (see the provisioner); remove the warning when the behaviour lands.
- `Config.EveryAlertTargets` is temporary, like `Pipeline.NotifyAll`: until routing and escalation exist, every alert notifies the channel targets in the first step of the default escalation policy.
- A new field needs a `yaml` tag and a `json` tag (the JSON form is what is stored in Postgres), and a case in `TestLoadReportsEveryStructuralError`. Unknown fields are rejected, so a field cannot be added by accident. A type with its own `UnmarshalYAML` reports problems with `typeError`, which lets the loader report every problem in a file and name the field.
- `config.Read` loads and validates in one call, and is what commands use. Every check of what a value means goes in `internal/config/validate.go`, reports through `object.errorf` with the path of the field, and gets a case in `testdata/invalid` (regenerate `testdata/invalid.golden` with `go test ./internal/config -run TestReadReportsEveryProblem -update`, and read the difference). Validation must never need a database.
- Errors are `config.Errors`: each names the file, line, object and field. Never put the value of a secret field into a message, and never repeat a value that the decoder rejected; it may be a secret typed into the wrong field.
- `docs/configuration.md` is the user's reference for the files, and `examples/` holds configurations that a test keeps valid. Update both with the schema.
- Hold every credential in a `config.Secret`. In a file it is a reference, `{env: NAME}` or `{file: /path}`, never the value. It prints, logs and encodes as that reference; call `Reveal` only at the point of use. A new secret field also goes into `resolveSecrets` in `internal/config/load.go`. Add every new secret to `secretSettings` and `secretsConfig` in `internal/cli/secrets_test.go`, which checks that no secret reaches help, errors or logs. A credential that is a process setting, such as the database URL, is registered with `secretSetting`.
- `provision.Reconcile` makes the provisioned rows match the configuration: it creates, updates and marks as removed, in one transaction under an advisory lock, and returns the row of every object. It never deletes; a removed object keeps its row for history and is revived if it returns. A new kind needs its table (with the common columns, including `provisioned`), a constant in `provision.Kinds` in dependency order, and a case in `objectsOf`.
- Rows with `provisioned = true` belong to the files. Nothing but reconciliation may write them: an API route for a provisioned kind is read-only.

### Sign-in and sessions

- `internal/auth` signs people in with the authorisation code flow. The protocol and the cryptography belong to [go-oidc](https://github.com/coreos/go-oidc) and `golang.org/x/oauth2`: never verify a token, build an authorisation URL or compare a signature by hand. `docs/access.md` is the guide; keep it true.
- Every sign-in uses a fresh state, nonce and PKCE verifier. The callback checks, in this order, the sign-in cookie against the state, the stored flow (used once, ten minutes), then the code and the ID token. Do not reorder them or make any of them optional.
- Session tokens and states are stored as SHA-256 hashes (`hashToken`), never as they are. Never log a token, a code, a state or a cookie value.
- Cookies are `HttpOnly` and `SameSite=Lax`, and `Secure` whenever `auth.public_url` is `https`. Routes that need a session go through `Handler.RequireSession`, which also refuses cross-site requests.
- Role and group-mapped team membership are decided in `grant` from the groups in the token, and applied by `Store.SignIn` at every sign-in as the whole truth: the role is overwritten and `team_members` rows of origin `group` are replaced. Never add to what an earlier sign-in gave.
- `team_members` rows of origin `config` follow the `members` lists in the files. `provision.Reconcile` sets them for every team, skipping addresses that no user has (`Result.Skipped`: logged as errors and counted in `beacon_team_members_skipped`, never a failure). `Store.SignIn` sets them for the one user who signs in, from the loaded configuration, which is how a skipped person joins. Each origin's code must filter on its own origin in every statement.
- A list that goes into `= ANY($n)` must never be a nil slice: pgx sends nil as `NULL`, and a comparison with `NULL` matches nothing, so a `NOT (... = ANY($n))` deletes nothing.
- API tokens (`internal/auth/tokens.go`) are 256 random bits behind the prefix `bcn_`, stored as a SHA-256 hash and returned once, by the request that creates them. A request with an `Authorization: Bearer` header is judged by the token alone and never falls back to the cookie. Managing tokens is for admins only, because a token can carry any role.
- The bootstrap admin (`internal/auth/bootstrap.go`) is the only password in Beacon. Beacon is given a bcrypt hash through a secret reference and compares with `bcrypt.CompareHashAndPassword`; do not add another way to set it, and do not log the password, a wrong guess or the hash. Its sign-in is public, so it does its own cross-site check and is bounded by `attemptLimiter`; keep both. Removing it from the configuration must end its sessions.
- `golang.org/x/crypto/bcrypt` is the only password hashing, and only for the bootstrap admin. Random tokens (sessions, API tokens, states) are hashed with SHA-256, which is right for secrets that cannot be guessed and wrong for anything a person chose.
- The identity provider is discovered lazily, so Beacon starts and handles alerts while it is down. Do not make the start depend on it.
- Tests that need a real provider use `oidctest` against the Dex in `compose.yaml` (`dev/dex/config.yaml` holds its users). Without `BEACON_TEST_OIDC_ISSUER` they are skipped locally and fail in CI. Use the hand-written provider in `internal/auth/fake_test.go` only for what a real one will not do, such as a token that is wrong in one way. Beacon has no account at any hosted provider; say in the PR that those are unverified.

### Ingestion

- `internal/ingest` serves `POST /api/v1/ingest/<source>`. The type of the source decides which payload is read: `docs/webhook.md` is the contract for the generic JSON payload, `docs/alertmanager.md` for Alertmanager's and `docs/grafana.md` for Grafana's. Every payload error must name the field at fault.
- A parser turns a payload into `delivery` values, one per alert, and `Handler.apply` hands each to the alert model. A new kind of source is a parser and a case in `ServeHTTP`; it must not reach the store by another path.
- The generic webhook is Beacon's own contract and rejects fields it does not know. The payloads of other systems are theirs: read only the fields Beacon uses, ignore the rest, and check the shape against the sender's current documentation and source, not memory. Say in the PR which version was implemented.
- `internal/ingest/testdata` holds notifications captured from the real senders. Capture new ones with the real sender when a payload changes, rather than editing them by hand. The Alertmanager and the Grafana under the Compose profiles `alertmanager` and `grafana` are set up for that.
- A sender repeats and retries. Whatever a receiver does must be safe to do twice, and a notification that could not be stored in full is answered with `500` so that it is sent again.
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

### End-to-end tests

- `internal/e2e` builds the binary, runs it as a process against a real Postgres and the fake Slack, and uses it only from outside: the webhook as a source would, the interactions endpoint as Slack would. It reads the database to check what was stored and never writes to it. The tests run with `go test ./...`, so CI runs them against the Postgres from `compose.yaml`.
- `TestFirstPath` is the path the product exists for: webhook in, Slack message out, acknowledgement from Slack, resolve from the source. A change that breaks it is not ready. Extend these tests when a feature changes that path, for example when routing decides where the message goes.
- Prefer a package test for a single behaviour; add an end-to-end test for a path that crosses packages.

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
