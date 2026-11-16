# AGENTS.md

Operational expectations for humans and AI agents working in this repository.

## Purpose

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

The project is at an early stage: the foundations are in place, and no alert handling exists yet. `docs/requirements.md` is the source of truth for what Beacon must do, which requirements are in the MVP, and the design decisions already made. Read it before proposing designs or writing code, and update it in the same PR when a decision changes.

## Tech stack

Per `docs/requirements.md`:

- **Go** for the server, shipped as a single binary (`cmd/beacon`). The command line uses the standard library `flag` package; there is no CLI framework.
- **Postgres** as the only required dependency, through [pgx](https://github.com/jackc/pgx) (`pgxpool`). Schema migrations are [goose](https://github.com/pressly/goose) SQL files embedded in the binary.
- **Declarative configuration files** for provisioned objects (sources, integrations, routing, escalation policies, teams, schedule rotations); the database holds runtime state (planned)

- **HTTP**: the standard library `net/http` server and router, `log/slog` for structured logs, and [client_golang](https://github.com/prometheus/client_golang) for `/metrics`. There is no web framework.

Tooling:

- **golangci-lint** for linting and formatting checks, configured in `.golangci.yml`
- **Renovate** for dependency updates (managers: `asdf`, `docker-compose`, `github-actions`, `gomod`)
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
│   ├── cli/                  # Subcommand dispatch and the subcommands themselves
│   ├── logging/              # slog setup; request identifiers in log records
│   ├── postgres/             # Connection pool and migrator
│   │   ├── migrations/       # Versioned SQL migrations, embedded in the binary
│   │   └── postgrestest/     # Test helper: one empty database per test
│   ├── server/               # HTTP server, middleware, health, readiness and metrics endpoints
│   └── version/              # Build version reporting
├── .golangci.yml             # golangci-lint configuration
├── .tool-versions            # Pinned language/tool versions (asdf/mise)
├── compose.yaml              # Postgres for development, version pinned
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

CI (`.github/workflows/ci.yml`) runs the build, lint, test and module checks on every pull request, whatever its base branch. It starts Postgres from `compose.yaml`, so CI and development use the same Postgres version.

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
- Release automation lives in `.github/workflows/release.yaml`: it computes the next version, tags, and creates the GitHub release. Building and publishing artifacts (images, binaries) is still a TODO stub in the workflow.

## Docs

- Project documentation lives in `docs/` as markdown and is the source for the project's docs site. Do not introduce another static site generator (MkDocs, Docusaurus, etc.) — docs are built with [nebula-md](https://github.com/thanoslabs/nebula-md).
- `.github/workflows/docs.yaml` calls nebula-md's reusable workflow (`thanoslabs/nebula-md/.github/workflows/deploy.yml`) on pushes to `main` that touch `docs/`. Generation happens in CI only — do not commit built HTML.
- Deployment to GitHub Pages runs only when the repository is **public**; private repos skip the job. Public repos must set **Settings → Pages → Source** to **GitHub Actions**.
- Pages are Obsidian-flavoured markdown: YAML frontmatter (`title`, `tags`, `date`), `[[wikilinks]]` between pages, and callouts. `docs/index.md` is the site's landing page.
