# AGENTS.md

Operational expectations for humans and AI agents working in this repository.

## Purpose

Beacon is an open-source alerting and on-call platform. It takes alerts from Prometheus-style sources and gets them to the right on-call person, with schedules, escalation and acknowledgement in one tool that is easy to run.

The project is at an early stage: provisioned objects are read from YAML files, people sign in through an OpenID Connect identity provider, alerts are received from Alertmanager, Grafana alerting or over a generic webhook, stored, routed by label, grouped and posted to the Slack channel of their escalation policy, where people can acknowledge, snooze and resolve them. Schedules run their rotations with overrides, and escalation policies page the person on call, teams and channels step by step until someone responds. The same works in Discord, where people link their accounts themselves, and alert groups are also sent as email and posted to outbound webhooks. The web interface is a single-page app in `web/`, embedded in the binary; people sign in to it, act on alerts, see who is on call, arrange overrides and silences, read what the configuration defines, link their chat accounts and, as admins, manage API tokens. `docs/requirements.md` is the source of truth for what Beacon must do, which requirements are in the MVP, and the design decisions already made. Read it before proposing designs or writing code, and update it in the same PR when a decision changes.

## Tech stack

Per `docs/requirements.md`:

- **Go** for the server, shipped as a single binary (`cmd/beacon`). The command line uses the standard library `flag` package; there is no CLI framework.
- **Postgres** as the only required dependency, through [pgx](https://github.com/jackc/pgx) (`pgxpool`). Schema migrations are [goose](https://github.com/pressly/goose) SQL files embedded in the binary.
- **Declarative configuration files** in YAML for provisioned objects (sources, integrations, routing, escalation policies, teams, schedule rotations), read with [go.yaml.in/yaml/v3](https://github.com/yaml/go-yaml); the database holds runtime state

- **HTTP**: the standard library `net/http` server and router, `log/slog` for structured logs, and [client_golang](https://github.com/prometheus/client_golang) for `/metrics`. There is no web framework.
- **Web interface** (`web/`): [React](https://react.dev/) and TypeScript, built by [Vite](https://vite.dev/) into `internal/web/dist` and embedded in the binary with `go:embed`. The API client is typed from the OpenAPI description ([openapi-typescript](https://openapi-ts.dev/) and its `openapi-fetch`), server state is held by [TanStack Query](https://tanstack.com/query), and pages are routed by [React Router](https://reactrouter.com/). The style sheet is written by hand; there is no component library. Tests are [Vitest](https://vitest.dev/) with Testing Library, and [Playwright](https://playwright.dev/) with axe against the real binary.

Tooling:

- **golangci-lint** for linting and formatting checks, configured in `.golangci.yml`
- **ESLint** (with typescript-eslint) for the web interface, configured in `web/eslint.config.js`
- **Renovate** for dependency updates (managers: `asdf`, `docker-compose`, `dockerfile`, `github-actions`, `gomod`, `npm`)
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
│   ├── scripts/              # The checks of the CI job `validate`, and a script that shows they can fail
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
│   ├── mailpit/              # The one account of the SMTP server used in development and tests
│   └── dex/                  # Configuration of the identity provider used in development and tests
├── docs/                     # Project documentation (markdown source for the docs site); images/web holds its screenshots of the web interface
├── examples/                 # Example configurations; a test keeps them valid
├── internal/
│   ├── access/               # Roles and the principal behind a request
│   ├── alert/                # Alert model, state machine and timeline
│   ├── api/                  # REST API: its conventions, the routes of alerts, read access to what is provisioned, and the OpenAPI description
│   │   └── apitest/          # Test helper: checks answers against the OpenAPI description
│   ├── auth/                 # Sign-in through OpenID Connect, users and sessions
│   │   └── oidctest/         # Test helper: sign in through the Dex of compose.yaml
│   ├── cli/                  # Subcommand dispatch and the subcommands themselves
│   ├── config/               # Typed provisioned objects, the YAML loader and the Secret type
│   ├── discord/              # Discord channel: HTTP API client, message rendering, button presses and the link command
│   │   └── discordtest/      # Fake Discord for tests
│   ├── e2e/                  # End-to-end tests: the real binary, Postgres, a fake Slack, a fake Discord and a real SMTP server
│   ├── email/                # Email channel: SMTP delivery and message composition
│   │   └── mailpittest/      # Test helper: the Mailpit of compose.yaml
│   ├── identity/             # Which Slack or Discord account belongs to which user: lookup by email, link codes
│   ├── ingest/               # Receivers for Alertmanager, Grafana and the generic webhook: POST /api/v1/ingest/<source>
│   ├── logging/              # slog setup; request identifiers in log records
│   ├── notify/               # Notification queue, workers, retries and the Channel interface
│   ├── postgres/             # Connection pool and migrator
│   │   ├── migrations/       # Versioned SQL migrations, embedded in the binary
│   │   └── postgrestest/     # Test helper: one empty database per test
│   ├── provision/            # Reconciles provisioned objects into Postgres
│   ├── routing/              # Routing rules: which team, service, policy and grouping an alert gets
│   │   └── provisiontest/    # Test helper: a provisioned row to reference
│   ├── schedule/             # On-call rotations: handoff arithmetic, positions and recorded shifts
│   ├── server/               # HTTP server, middleware, health, readiness and metrics endpoints
│   ├── silence/              # Silences: matchers, the store and the API
│   ├── slack/                # Slack channel: Web API client, message rendering and button presses
│   │   └── slacktest/        # Fake Slack for tests
│   ├── version/              # Build version reporting
│   ├── web/                  # Serves the web interface: the embedded build, its headers, and the fallback to the page
│   │   └── dist/             # Where `npm run build` writes; only .gitkeep is in git
│   └── webhook/              # Outbound webhook channel: signed JSON to an address of the operator's
├── web/                      # The web interface: a single-page app (React, TypeScript, Vite)
│   ├── e2e/                  # Browser tests (Playwright) against the real binary, their configuration and the script that runs Beacon for them
│   ├── public/               # Files served beside the page as they are
│   └── src/
│       ├── api/              # The typed API client; schema.d.ts is generated from internal/api/openapi.yaml
│       ├── app/              # The frame, the session, the theme and the shared pieces
│       ├── pages/            # One file per page
│       └── test/             # Helpers of the component tests: a fake API and a render function
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
| Build the web interface | `(cd web && npm ci && npm run build)`: optional for work on Go alone; without it the binary has no app and says so |
| Build | `go build -o bin/beacon ./cmd/beacon` |
| Run | `./bin/beacon version` (or `go run ./cmd/beacon version`) |
| Start Postgres | `docker compose up -d --wait postgres` |
| Start Postgres and the identity provider | `docker compose up -d --wait postgres dex` |
| Start everything the tests use | `docker compose up -d --wait postgres dex mailpit` |
| Test | `BEACON_TEST_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' BEACON_TEST_OIDC_ISSUER='http://127.0.0.1:5556/dex' BEACON_TEST_SMTP_ADDR='127.0.0.1:1025' BEACON_TEST_MAILPIT_URL='http://127.0.0.1:8025' go test -race ./...` |
| Run the server | `BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' BEACON_SOURCE_TOKEN=local-development-token ./bin/beacon serve --config examples/quickstart` then `curl localhost:8080/readyz` |
| Check configuration | `./bin/beacon validate --config examples/full` |
| Preview schedule changes | `BEACON_DATABASE_URL=... ./bin/beacon preview --config examples/full` |
| Hash a password for the bootstrap admin | `./bin/beacon hash-password` |
| Apply migrations | `BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable' ./bin/beacon migrate` |
| End-to-end tests only | `BEACON_TEST_DATABASE_URL=... go test -race -count=1 ./internal/e2e/` |
| Lint | `golangci-lint run ./...` |
| Fix formatting | `golangci-lint fmt ./...` |
| Check module files | `go mod tidy -diff` |
| Regenerate the published API description | `go test ./internal/api -run TestPublishedDescription -update` |
| Web interface: type check, lint, unit tests | in `web/`: `npm run typecheck`, `npm run lint`, `npm test` |
| Regenerate the API types of the web interface | in `web/`: `npm run generate` (CI runs `npm run generate:check`) |
| Browser tests | `docker compose up -d --wait postgres dex mailpit`, build the web interface and `bin/beacon`, then in `web/`: `npx playwright install chromium` once and `npx playwright test` |
| Work on the web interface | in `web/`: `npm run dev:beacon` (a Beacon on `127.0.0.1:18080` with the Dex users) and `npm run dev` (the app on `http://127.0.0.1:5173`) |
| Retake the screenshots of the docs | in `web/`: `BEACON_SCREENSHOTS=1 npx playwright test screenshots`, then look at `docs/images/web` |
| Build the image | `docker build -t beacon:dev .` |
| Run Beacon and Postgres | `docker compose up --build -d --wait`, then `curl localhost:8080/readyz` (set `BEACON_PORT` if 8080 is taken) |
| Run them with an Alertmanager that sends to Beacon | `BEACON_CONFIG_DIR=./examples/alertmanager docker compose --profile alertmanager up --build -d --wait` (set `BEACON_ALERTMANAGER_PORT` if 9093 is taken) |
| Run them with a Grafana that sends to Beacon | `BEACON_CONFIG_DIR=./examples/grafana docker compose --profile grafana up --build -d --wait` (set `BEACON_GRAFANA_PORT` if 3000 is taken), then create the rule as `docs/grafana.md` shows |

CI (`.github/workflows/ci.yml`) runs the build, lint, test and module checks on every pull request, whatever its base branch. Its `validate` job refuses committed files that are sensitive by name or hold private key material, and YAML that does not parse (`.github/scripts`); `test-checks.sh` runs there too and shows that both checks fail when they should, so change a check and its cases together. It builds the web interface before the Go code, so the binary under test has the app in it. It starts Postgres, Dex and Mailpit from `compose.yaml`, so CI and development use the same versions. The tests include the end-to-end tests in `internal/e2e`. The `web` job checks that the generated API types are fresh, then type checks, lints, tests and builds the app; the `browser` job runs the Playwright tests against the real binary. A separate job builds the image, starts Beacon and Postgres with Compose and the configuration in `examples/quickstart`, and checks the endpoints, that the webhook accepts an alert, that the image serves the web interface, the non-root user and a clean exit on termination. It then starts the Alertmanager and the Grafana of the Compose profiles `alertmanager` and `grafana` and checks that an alert of each fires and resolves in Beacon.

### Container image

- The Go version appears in `.tool-versions` and in the `Dockerfile` build stage. Change both together; CI fails when they differ, and Renovate groups them as "go toolchain". The same holds for the Node version and the stage that builds the web interface ("node toolchain").
- The stage that builds the web interface runs on the build platform, like the Go build stage: what it builds is the same for every platform. The Go stage refuses to go on without `internal/web/dist/index.html`, so an image without the app cannot be built.
- `.dockerignore` excludes everything except what the `Dockerfile` copies. A new top-level source directory must be added to both files.
- The final stage has no `RUN` step, so building for another platform needs no emulation. Keep it that way, or add QEMU to the release workflow.
- The image runs as the numeric user `65532`. Do not write to the container file system at runtime.

### Server

- `beacon serve` reads and validates the configuration, applies pending migrations (unless `--migrate=false`), then serves `GET /healthz`, `GET /readyz` and `GET /metrics` on `--listen-address` (default `:8080`).
- Attach a route with `Server.Handle(access.Route{Pattern, Level, Handler})`; there is no other way to the router. Every route passes through the request identifier, access log and panic recovery middleware, and every route that is not `access.Public` through the guard in `internal/access`: `401` without a valid credential, `403` for a cross-site request that changes something, `403` for a member on an `access.Admin` route. The handler finds who is asking with `access.FromContext`.
- **Beacon is closed by default.** A new route is `access.Member` or `access.Admin`. The zero value of the level is `Admin`, so a forgotten level is closed. `access.Public` is only for a route that a machine without a session must reach, and then the handler must check a credential of its own (the source's token, Slack's signature) unless the route is harmless to a stranger. Every route of `beacon serve` is listed in `provisioner.routes` in `internal/cli/app.go` and, on purpose, a second time in `wantRoutes` in `internal/cli/routes_test.go`: adding a route means writing its level down twice and justifying a public one in the comment there.
- **The API has one style**, written down for users in `docs/api.md` and for code in `internal/api/api.go`: a list is an object with one named array, and `next_cursor` when it is paged (`api.Limit`, `api.Cursor`, `api.ParseCursor`); an error is `{"error", "field"}` (`api.Fail`); `400` names the field at fault, `404` also answers an identifier of the wrong form, `409` is for what the state does not allow; times are RFC 3339 in UTC; an absent value is `null`, never an empty string, and an array is never `null`; a parameter or a body field that the route does not know is refused. A new route follows it, and a route that cannot says why in `docs/api.md`.
- **The OpenAPI description is `internal/api/openapi.yaml`**, written by hand, embedded and served at `/api/v1/openapi.json` and `.yaml`. Three tests hold it to the server, and a change to a route or to an answer goes through all three:
  - `TestRoutes` compares it with `wantRoutes`: every route is described, with its level as `x-beacon-access` (and `security: []` exactly when public), and nothing else is.
  - `TestAnswersMatchTheOpenAPIDescription` (`internal/cli/openapi_test.go`) uses every route of the real server and checks each answer with `apitest`: the status must be described and the body must fit its schema. A new route must be used there until it is seen to succeed; the few that cannot be are listed in `notChecked` with the reason.
  - `TestPublishedDescription` keeps `docs/openapi.yaml` and `docs/api-reference.md` equal to it; regenerate them with `go test ./internal/api -run TestPublishedDescription -update`.
- Every object schema of an answer says `additionalProperties: false` and lists what is `required`, which is what makes an undescribed field fail. `apitest` understands a small part of JSON Schema on purpose and refuses a keyword it does not check; teach it the keyword rather than loosen a schema.
- A handler that changes something takes who is acting from `access.FromContext`, never from the request, and logs the change with `by_user` and `by_token`.
- Log with the `slog` methods that take a context (`InfoContext` and so on) inside request handling, so the record carries `request_id`.
- Register metrics on the registry from `server.NewRegistry()`, not the global default. Keep label values to bounded sets (see the cardinality warning in `docs/requirements.md`).
- The process must stay stateless: anything that has to survive a restart or be seen by another replica goes in Postgres, never in memory or on local disk.

### Web interface

- `web/` is the app and `internal/web` serves it. `npm run build` writes into `internal/web/dist`, which `go:embed` takes into the binary. **Go must build and test without Node**: only `dist/.gitkeep` is in git, a binary without the app says so (in the log and to a browser), and no Go test may need the built app except `TestEmbedded`, which is skipped without it and fails in CI. `docs/web.md` is the guide; keep it true, and retake its screenshots when a page changes.
- **The route of the app is public and takes every `GET` that no other route takes** (`web.Pattern`). That is safe only while two things hold, and `TestRoutes` and `internal/web/web_test.go` check both. The files hold nothing of an install: never render configuration, a version that matters, or anything from the database into the page; the app asks the API. And `web.Reserved` gives every address under `/api` and `/auth` back to the server, so an unknown API address is `401` and JSON, never the page.
- Every answer of the handler carries `web.ContentSecurityPolicy` and the other headers. The policy allows nothing inline and no other host: do not loosen it for a library; choose a library that needs no inline script or style, and serve fonts and images from the binary. Files under `assets/` have a hash in their name and are immutable; the page is `no-cache`.
- **Alert text is attacker-controlled**: labels, annotations and links are written by whoever can send an alert. Render them as text, only ever through JSX; ESLint refuses `dangerouslySetInnerHTML` and `innerHTML`. A link from an alert is an `<a>` only when its scheme is `http` or `https`.
- **The session is the cookie, and nothing else.** No token, and nothing that stands for the session, goes into `localStorage`, `sessionStorage` or a readable cookie. The browser's storage holds the theme and, during a sign-in, the path to return to (`takeReturnTo` accepts only a path of the app).
- The app calls the API only through `api` in `web/src/api/client.ts`, typed from `web/src/api/schema.d.ts`, which is generated: change `internal/api/openapi.yaml`, run `npm run generate`, commit both. Never edit the generated file or write a type of an answer by hand. `unwrap` turns an answer into its body or an `ApiError` with the API's own message, and a `401` from any route but `/api/v1/me` puts the sign-in page back.
- The app decides what to *offer* by the role (`useIsAdmin`); what a person may *do* is decided by the API. Never rely on a hidden button, and when a view is for admins, a test must show that a member neither sees it nor gets its data.
- The views of alerts ask again at `pollInterval` (the API has no push channel), keep what they have when a refresh fails and say so, and offer exactly the actions the API lists for an alert (`actions`): the app never works out by itself what a state allows.
- **Times are shown in the viewer's zone** (`formatDateTime`), with the zone named. A view of a schedule also states the schedule's own zone. Weeks and days are found from calendar dates in `web/src/pages/schedules/calendar.ts`, never by adding hours, and its tests run in a zone with clock changes (`TZ` in `vite.config.ts`).
- The app never works out who is on call: it shows `on_call` and the shifts the API answers, which already hold overrides and pending changes.
- A form sends what was typed and lets the API judge it: the app does not repeat the API's rules. A refusal is shown with the API's message, and the field the API names is marked (`aria-invalid`). A change that takes something away, such as removing an override, asks first in a dialog.
- The app never matches alerts against matchers by itself: the preview of a silence asks `POST /api/v1/silences/preview`, so that what is previewed is what will be silenced.
- A secret that the API answers once (a new API token, a link code) lives in a component's state and nowhere else: not in the query cache, not in browser storage, not in the address. It is gone when the person puts it away or leaves the page.
- A control with a visible word and an `aria-label` that says more ("Revoke" and "Revoke the token deploy") must begin its label with the visible word; axe fails it otherwise.
- What the configuration defines is read-only in the app: such a view says that the object is managed in the configuration files and offers no control that changes it. Build one with `ConfigurationPage` (`web/src/pages/configuration/Shell.tsx`), and keep its test's `expectReadOnly`.
- Everything is served from the binary: no fonts, scripts, images or requests to another host, and no telemetry.
- Accessibility is part of done: semantic elements, a label for every control, a visible focus ring, text contrast of at least 4.5:1 in both themes, and a layout that works at 390 pixels. A new view gets `expectAccessible` in a browser test, which runs axe in both themes and at phone width.
- Runtime dependencies are few on purpose (React, React DOM, React Router, TanStack Query, `openapi-fetch`). Adding one needs a reason in the PR. Versions are exact in `package.json`, and the lockfile is committed.
- Component tests (`*.test.tsx`) use `fakeApi` from `web/src/test/api.ts` in place of the API and query by role and label. Browser tests (`web/e2e`) use the real binary from outside, wait for what they expect to see and never for a time, never retry, and fail on any violation of the Content-Security-Policy.

### Database

- Settings are read from flags first, then environment variables (`BEACON_DATABASE_URL`, `BEACON_DATABASE_MAX_CONNS`, `BEACON_DATABASE_CONNECT_TIMEOUT`). Register new settings with the helpers in `internal/cli/flags.go` so both forms always exist.
- Tests that need a database call `postgrestest.New(t)` or `postgrestest.NewMigrated(t)`. Without `BEACON_TEST_DATABASE_URL` they are skipped locally and fail in CI. Always run them against a real Postgres before claiming a database change works; do not mock the database.
- Add a migration as `internal/postgres/migrations/<next 5-digit number>_<snake_case>.sql` with `-- +goose Up` and `-- +goose Down` sections. Never edit a migration that has been released; add a new one. Each migration runs in a transaction, and a Postgres advisory lock keeps replicas from applying one twice.
- The schema follows `docs/configuration-and-data-model.md`. If a migration departs from it, update that page in the same PR.

### Alerts

- `internal/alert` owns the `alerts`, `alert_groups` and `alert_events` tables. Change alert state only through `alert.Store`, so that every change passes the state machine (`alert.Next`) and is written with its timeline event in one transaction. `docs/alert-lifecycle.md` describes the states.
- One open alert per source and fingerprint is kept by two things, and both must stay: `lockFingerprint` makes concurrent deliveries of one alert take turns, so that none fails, and the unique index `alerts_open_fingerprint_key` refuses a duplicate whatever the code does. A repeated delivery must not write an event or queue a notification.
- Every alert is in a group (`internal/alert/group.go`). `Firing.Group` names the group a new alert joins; the ingest handler builds it from the `group_by` labels. A group's timers are the columns `notify_at`, `notified_at` and `update_interval` of `alert_groups`, never state in the process: `joinGroup` and `afterResolve` set them, and `Store.NotifyDueGroup` hands a due group to one caller under `FOR UPDATE SKIP LOCKED`. `afterResolve` locks the group row in a statement of its own before it decides whether the group is resolved; without that an alert that joins at the same moment can be left open in a resolved group.
- The API reads alerts through `internal/alert/query.go` (`List`, `View`, `Events`), which joins the names of what an alert refers to, and changes them through `Store.Apply` and `Store.ApplyToGroup` like chat does, as `alert.User` or `alert.APIToken`. A list is ordered by (`started_at`, `id`), newest first, and continues from an `alert.Position`: keep the order total, or pages skip and repeat.
- To react to a state change, register an `alert.Observer`. It runs inside the transaction that makes the change, so what it writes commits or rolls back with it.
- The store takes the time from its clock, not from Postgres, so tests can move time with `SetClock`.
- Background work started by `beacon serve` watches the command's context and is waited for before the command returns. Anything it needs to remember goes in Postgres.

### Schedules

- `internal/schedule` owns the `schedule_positions` and `shifts` tables. `docs/schedules.md` describes the rules; keep it true. The file is the intent and the database is the position: nothing may work out who is on call from the files alone.
- The arithmetic is pure and lives in `rotation.go`: `Start`, `State.WithDefinition` and `State.Next` say what happens, and `Store` only applies them. `beacon preview` runs the same functions ahead of time (`State.Project`). Change the rules there and nowhere else, so that the preview cannot disagree with what the store will do; `TestPreviewIsWhatHappens` compares the two shift for shift.
- A change of definition never moves the shift in progress. `Store.Apply` stores it as pending, and it takes over in `State.Next`: at a handoff, or at the stated time, where the person on call stays unless the new rotation no longer has them. Do not write `definition` anywhere else.
- An override (`override.go`) is runtime state, created and removed through the API and never from the files. Overrides of one schedule may overlap, and are layered: where they do, the one created last (`schedule_overrides.layer`) is in force, and removing it reveals the one beneath. `overrideLayers` is the one place that resolves that; `Store.OnCall`, `Store.Timeline` and the preview all go through it, and `TestOverridesAreLayered` checks that they agree. Never decide between two overrides anywhere else, and never by a timestamp. An override never touches the position: the rotation goes on underneath, and the override's shift is recorded beside the rotation's.
- `Store.Timeline` answers who is on call over a period: closed shifts for the past, `State.Project` and the stored overrides for the rest, flattened so that there is one person at any moment. `TestTimelineIsWhatHappens` compares the plan with the record it becomes.
- `Store.OnCall` is the one answer to "who is on call now", and `OnCallForTeam` asks it for every schedule a team owns. It applies handoffs that are due but not yet carried out, and reads overrides directly, so it never depends on how recently the worker ran. Do not read `schedule_positions.user_id` for that anywhere else.
- The schedule routes follow the conventions of silences: a list is `{"overrides": [...]}`, a creation answers `201` with the object, `DELETE` answers `204`, an error is `{"error", "field"}`, and the author comes from `access.FromContext`, never from the body.
- `beacon preview` must stay read-only (it runs in a read-only transaction) and must not need the secrets of the configuration.
- A handoff instant is always found from a calendar date in the schedule's zone (`instant`), never by adding hours. Any change to the arithmetic needs cases in `TestDaylightSaving` for both directions in a northern and a southern zone.
- A shift is recorded when it begins and closed once. Never update or delete a closed shift; a trigger refuses the update. A handoff is recorded at the instant it was due, not at the time the worker ran.
- Read a row that is locked with `FOR UPDATE` from its own table alone, and load what belongs to it in further statements. A locking read that waited sees the locked row as the other transaction left it and the joined rows as they were before, which once made the wrong person hand over; `TestReplicasHandOverOnce` is the guard. For the same reason, a claim under `SKIP LOCKED` is only safe when its condition is on the claimed row and the claimer changes that row: a replica that takes the row just after another let go of it then sees the change. When the condition is on another table, as for an override that has no shift yet, ask again once the row is held (`recordBegun`).
- A schedule with nobody on call has a gap in coverage, which is a legitimate state and not a fault: log it as a warning (`schedule.GapInCoverage`), never as an error, and let an escalation step that meets it skip the target with that reason and go on.
- The store takes the time from its clock (`SetClock`), never from Postgres. The provisioner calls `Store.Apply` after reconciliation, which starts new schedules and ends removed ones; the worker (`Store.Run`) carries out handoffs. A completed sign-in makes one pass of the worker itself (`Store.AdvanceAndReport`), so that the first member of a rotation to become a user is on call at once; it is the same `Advance`, and nothing else may be added to a sign-in to move a position.

### Configuration and secrets

- Provisioned objects are typed in `internal/config`, following `docs/configuration-and-data-model.md`. `config.Load` reads them from one YAML file or a directory, and `beacon serve` takes that path as `--config`. The files are the only route into the features: do not add a flag or an environment variable for something that is provisioned. Process settings, such as the listen address, stay flags.
- Everything that follows the configuration is applied by the `provisioner` in `internal/cli/app.go`, at start and on every reload (`SIGHUP`). It reconciles first and swaps afterwards, so a failure leaves the previous configuration active. A feature that reads provisioned objects must take them through a setter that is safe while requests run (`ingest.Handler.SetSources`, `notify.Pipeline.SetChannels`), called from the provisioner; never capture configuration at start. `TestReload` in `internal/e2e` is the guard: extend it when a feature starts to follow the configuration.
- A kind or type can be valid in the files before it does anything. `beacon serve` then stores it and logs a warning that names it, which is removed when the behaviour lands. Every type of source and of integration does something today.
- A new field needs a `yaml` tag and a `json` tag (the JSON form is what is stored in Postgres), and a case in `TestLoadReportsEveryStructuralError`. Unknown fields are rejected, so a field cannot be added by accident. A type with its own `UnmarshalYAML` reports problems with `typeError`, which lets the loader report every problem in a file and name the field.
- `config.Read` loads and validates in one call, and is what commands use. Every check of what a value means goes in `internal/config/validate.go`, reports through `object.errorf` with the path of the field, and gets a case in `testdata/invalid` (regenerate `testdata/invalid.golden` with `go test ./internal/config -run TestReadReportsEveryProblem -update`, and read the difference). Validation must never need a database.
- Errors are `config.Errors`: each names the file, line, object and field. Never put the value of a secret field into a message, and never repeat a value that the decoder rejected; it may be a secret typed into the wrong field.
- `docs/configuration.md` is the user's reference for the files, and `examples/` holds configurations that a test keeps valid. Update both with the schema.
- Hold every credential in a `config.Secret`. In a file it is a reference, `{env: NAME}` or `{file: /path}`, never the value. It prints, logs and encodes as that reference; call `Reveal` only at the point of use. A new secret field also goes into `resolveSecrets` in `internal/config/load.go`. Add every new secret to `secretSettings` and `secretsConfig` in `internal/cli/secrets_test.go`, which checks that no secret reaches help, errors or logs. A credential that is a process setting, such as the database URL, is registered with `secretSetting`.
- `provision.Reconcile` makes the provisioned rows match the configuration: it creates, updates and marks as removed, in one transaction under an advisory lock, and returns the row of every object. It never deletes; a removed object keeps its row for history and is revived if it returns. A new kind needs its table (with the common columns, including `provisioned`), a constant in `provision.Kinds` in dependency order, and a case in `objectsOf`.
- Rows with `provisioned = true` belong to the files. Nothing but reconciliation may write them: an API route for a provisioned kind is read-only.
- `api.Provisioned` answers the provisioned objects from the active configuration, which the provisioner hands it (`SetConfig`), through types of its own, never by encoding a `config` type. **It answers no credential and no reference to one**: a new field of an integration is added to `settings` only when it is not a secret, and the address of a webhook goes out through `withoutSecrets`. `TestProvisionedObjectsAreReadable` reads every route with every secret set.

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

### Chat identities

- `internal/identity` owns `user_identities`, `identity_lookups` and `identity_link_codes`. `docs/access.md` (Chat accounts) and `docs/slack.md` (People) describe it; keep them true.
- An identity belongs to a user and an integration. An account is never moved from one user to another: `Store.Link` returns `ErrTaken`, and callers log it and leave things as they are.
- `Linker` finds Slack accounts by email address. Whom it has asked about is in `identity_lookups`, claimed in the statement that selects them, so replicas do not repeat each other and Slack's rate limit is respected; do not replace it with a cache in the process. A failed lookup ends the pass for that integration.
- A new chat service is an `identity.Integration` that the provisioner builds: with a `Lookup` when the service can be asked by email address, without one when people link their accounts themselves (`StartLink`, `CompleteLink`).
- Link codes are random, short-lived and stored as SHA-256 hashes, like session tokens, and never logged. `CompleteLink` trusts its caller to have verified that the request comes from the chat service and from that account; a route that calls it must check the service's signature first. `POST /api/v1/me/identities/{integration}/link-code` issues a code to the signed-in person, and only to a person: not to a token and not to the bootstrap admin. `DELETE /api/v1/me/identities/{integration}` removes a link the person made; a link that Beacon found by email address is not theirs to remove, because Beacon would find it again.
- Whoever acts from chat is identified in one place per service, `Interactions.identify` in `internal/slack` and in `internal/discord`: the linked user when there is one, the chat account alone otherwise. **Only a linked user may acknowledge or snooze; anyone may resolve.** A refused press changes nothing and is answered with an ephemeral note that says how to get linked. When the lookup itself fails, acknowledging and snoozing are refused with "try again" and resolving goes ahead: never guess that someone is linked.

### Ingestion

- `internal/ingest` serves `POST /api/v1/ingest/<source>`. The type of the source decides which payload is read: `docs/webhook.md` is the contract for the generic JSON payload, `docs/alertmanager.md` for Alertmanager's and `docs/grafana.md` for Grafana's. Every payload error must name the field at fault.
- A parser turns a payload into `delivery` values, one per alert, and `Handler.apply` hands each to the alert model. A new kind of source is a parser and a case in `ServeHTTP`; it must not reach the store by another path.
- The generic webhook is Beacon's own contract and rejects fields it does not know. The payloads of other systems are theirs: read only the fields Beacon uses, ignore the rest, and check the shape against the sender's current documentation and source, not memory. Say in the PR which version was implemented.
- `internal/ingest/testdata` holds notifications captured from the real senders. Capture new ones with the real sender when a payload changes, rather than editing them by hand. The Alertmanager and the Grafana under the Compose profiles `alertmanager` and `grafana` are set up for that.
- A sender repeats and retries. Whatever a receiver does must be safe to do twice, and a notification that could not be stored in full is answered with `500` so that it is sent again.
- Receivers change alerts only through `alert.Store`.

### Routing

- `internal/routing` turns the routing block into a `Router`. The ingest handler asks it for the `Route` of every delivery and hands the result to `alert.Store.Fire`, which keeps it only when the delivery opens an alert: an open alert is never routed again. The provisioner builds the router before it swaps anything, so a router that cannot be built leaves the old configuration active.
- Patterns from users, in routing rules and in silences, are compiled with `config.CompileMatcher` and nothing else: it anchors the pattern at both ends, as Prometheus does, and bounds its length. Go's `regexp` is RE2, which matches in linear time; do not replace it with an engine that backtracks.
- The group key has the routing rule as its scope (`alert.GroupKey`), so alerts of different rules never share a group.
- Metric labels stay bounded: `beacon_routing_unmatched_alerts_total` has the source and nothing else. A rule name, an alert name, a fingerprint or an identifier is never a label.

### Silences

- `internal/silence` owns the `silences` table and serves `/api/v1/silences`; `docs/silences.md` is the contract. A silence is runtime state: it is created through the API, never from the files.
- Everything in a silence comes from a request, so everything is bounded (`MaxMatchers`, `MaxValueLength` and the rest in `silence.go`), and a pattern is compiled with `config.CompileMatcher` like one in a routing rule. Check a silence in `compile` and `Store.Create`, not in the handler, so that no other way of creating one can skip the checks.
- The author is the principal of the request, a user or a token, taken from `access.FromContext`. Never read it from the body.
- A silence acts where a group's notification is decided, `Pipeline.notifyGroup`, and nowhere else: nothing is written on an alert when a silence matches it, so a silence that is created, ends or is expired needs no pass over the alerts. `alert_groups.notified_alerts` remembers what each notification showed, which is what makes "news" and "hidden" decidable, and `alert_groups.silence_id` names the silence a group waits for, which is what `Store.WakeGroupsWaitingFor` uses when `silence.Store.Expire` ends one early. The escalation engine must ask the same question (`Pipeline.quiet`) before each step it runs.
- The routes follow the tokens' conventions: a list is `{"silences": [...]}`, a creation answers `201` with the object, `DELETE` answers `204`, an error is `{"error", "field"}`, and an identifier that is not a UUID is `404` like one that does not exist.

### Notifications

- `internal/notify` owns the `notifications` and `notification_attempts` tables. `docs/notifications.md` describes the delivery rules; keep it true.
- Queue a notification with `Pipeline.Enqueue` inside the transaction of the change that causes it, never after the commit and never by sending directly.
- A new notification service is a type that implements `notify.Channel`, registered with `Pipeline.Register`. The pipeline must not learn anything specific to a service. What every channel says about a group (the state it is shown in, its title, the actions its alerts allow, who was paged) is worked out once, in `internal/notify/describe.go`; a channel only writes it out and escapes it for its service. A channel returns `notify.Permanent` for failures that retrying cannot fix and `notify.RetryAfter` when the service says how long to wait; anything else is retried with backoff. Channel errors are stored and shown on the timeline, so they must never contain a credential.
- Delivery is at least once. A channel should make a repeated `Send` harmless where the service allows it.
- Notifications are about groups, and `internal/notify/groups.go` decides when one is queued: what sources report is batched by the group's timer (`Pipeline.NotifyDueGroups`, run every second by `RunGroups`), and what people do is told at once by the observer `Pipeline.Observe`. Updates go to every destination that was told about the group before, and a destination with a notification still waiting gets no second one, because a message is built when it is delivered.
- A notification for one person carries their `UserID`, in the page and in every update that follows it (`enqueueUpdate`), and its destination is where the integration reaches them, resolved when it is queued. A channel for which a person and a shared destination differ, as in Discord, tells them apart by that.
- Tests of timing move the clock (`SetClock` on the store and the pipeline) and call `NotifyDueGroups` themselves. Do not sleep and do not wait for the ticker.

### Escalation

- The engine is `internal/notify/escalation.go`, and `docs/escalation.md` describes its rules; keep it true. It owns the `escalations` table: one row per group, whose `next_step_at` is the timer.
- `Pipeline.startEscalation` runs the first step, in the transaction that marks the group as notified. `Pipeline.EscalateDue` runs later steps: it claims a due row under `FOR UPDATE SKIP LOCKED`, reads the row from its own table alone, and moves the timer in the same transaction. `Pipeline.Observe` stops an escalation when no alert of its group is firing, in the transaction of the change, and restarts it when a snooze runs out. **A group is paged once**: an alert that joins a group whose escalation has stopped must not restart it or queue an `escalation_step`; the group's update edits the messages that exist. The opt-in re-page policy (issue 100) has its seam marked in `observeEscalation`. Do not add another place that writes `escalations`.
- A step asks `Pipeline.quiet` before it notifies, as a group's notification does: while every firing alert is silenced it waits for the silence, and `WakeEscalationsWaitingFor` wakes it when the silence is expired.
- The pipeline learns about people through `notify.People` and nothing else: who is on call (`schedule.Store.OnCall`), who is in a team, and where an integration reaches a person: the linked account of a chat service (`identity.Store.Address`), or, for an email integration, the address the user has in Beacon, which needs no link. The adapter is `internal/cli/people.go`. People are resolved when the step runs, inside its transaction.
- A person who has no linked account is skipped for delivery and stays visible: the `skipped` entry carries `not_connected` and `shown_as` (`notify.NotConnected`, "Grace Hopper (Slack not connected)"), and a channel that names who was paged shows that text, never a mention. The Slack message builds its "Paged:" line from the `escalated` events (`pagedLine`); a mention is only ever made from a destination that is a Slack user ID of the integration the message is sent with.
- A target that cannot be notified is skipped, logged (`escalation target not notified`) and listed in the `skipped` of the step's `escalated` event with a reason. It never fails the step. Every step that runs writes one `escalated` event.
- The steps are read from the policies of the active configuration each time (`SetPolicies`), never copied into the row, so a reload applies to escalations in progress. The provisioner resolves the names of schedules, teams and integrations to rows when it builds a `notify.Policy`.
- Tests move the clock (`SetClock`) and call `EscalateDue` themselves, like the tests of group timers. `TestEscalationTimersSurviveARestartAndFireOnce` is the guard for the claim.

### Slack

- `internal/slack` is the Slack channel. It calls four Web API methods, `chat.postMessage`, `chat.update`, `users.lookupByEmail` and `users.info`, with the standard library; there is no Slack SDK. The two `users` methods take their argument in the query, not as JSON, and the fake refuses to read it from a JSON body.
- Beacon has no Slack credentials for testing. Test against `slacktest`, the fake Slack, which checks requests against the documented shapes and limits. When you change what is sent, read Slack's current documentation rather than trusting memory, teach the fake the same rule, and say in the PR that real delivery is unverified.
- A notification with the reason `escalation_step` to a destination that already has the group's message posts a new message and turns the old one into a pointer (`superseded`): an edit notifies nobody. Every other notification edits in place.
- A person is paged by `chat.postMessage` with their user ID as the channel, which opens the app's direct conversation with them and needs the app's Messages tab.
- A message is about an alert group. `render` shows a group of one alert as that alert, and a larger group as what its alerts share and a bounded list of them; the state shown and the buttons offered are derived from all the alerts.
- Everything that comes from an alert passes through `escape` before it goes into message text, and each part is clipped so a message stays within Block Kit's limits.
- Button presses arrive at `POST /api/v1/integrations/<name>/interactions`. The handler verifies the Slack signature over the raw body before it parses anything; keep it that way. It answers within Slack's three seconds, and changes alerts only through `alert.Store.ApplyToGroup`, as the Beacon user the Slack account is linked to (`alert.LinkedChatUser`). An account that is linked to nobody may only resolve, as the Slack account alone (`alert.ChatUser`), named by `users.info` with "(Slack not connected)" (`Interactions.stranger`); that question to Slack is bounded by `profileTimeout` and its failure must never block or fail the resolve.
- The buttons a message offers are derived from the state machine (`alert.Next`), so they cannot drift from what is allowed.
- `docs/slack.md` is the setup guide. Update it when scopes or settings change.

### Discord

- `internal/discord` is the Discord channel. It calls four routes of Discord's HTTP API, version 10, with the standard library: Create Message, Edit Message, Create DM and Create Global Application Command. There is no Discord SDK and no gateway connection.
- Beacon has no Discord credentials for testing. Test against `discordtest`, the fake Discord, which checks requests against the documented shapes and limits. When you change what is sent, read Discord's current documentation rather than trusting memory (its source is the repository `discord/discord-api-docs`), teach the fake the same rule, and say in the PR that real delivery is unverified.
- A destination is a channel ID, or, when the notification carries a `UserID`, the ID of the person's Discord account: `Channel.open` then opens the direct message channel first. A reference is `channelID:messageID`.
- **No message may notify by a mention.** `allowed_mentions` is always `{"parse": []}`, people who were paged are named inside the embed, and everything that comes from an alert passes through `escape` (and `oneLine` where a line break would start markup) and is clipped to Discord's limits. An edit sends every part of the message, because Discord keeps what an edit leaves out.
- A 4xx answer other than 429 is permanent: Discord counts refused requests against the sender's address. The client remembers the rate limits Discord reports (`Client.remember`) and holds a request back with `notify.RetryAfter` rather than send one it knows will be refused. That memory is an optimisation of one process, not state.
- Interactions arrive at `POST /api/v1/integrations/<name>/interactions`, the route Slack uses too; `provisioner.handleInteractions` sends a request to the handler of the integration's type. `discord.Interactions` verifies the Ed25519 signature over the timestamp and the raw body, with `crypto/ed25519`, before it parses anything; keep it that way. Discord sends wrongly signed requests on purpose and wants `401` for them, and a `PING` wants a `PONG`.
- An interaction is answered in the HTTP response, within three seconds (`answerWithin` bounds the work): a deferred update when the action was taken, because the message is edited through the pipeline, and otherwise an ephemeral message. Alerts change only through `alert.Store.ApplyToGroup`, as the linked user (`alert.LinkedChatUser`) or, for a resolve by an account that is linked to nobody, as the Discord account alone, named as the interaction names it with "(Discord not connected)".
- The buttons are derived from the state machine (`notify.Actions`). A custom ID is the button's identifier, a colon and the group.
- `/beacon-link` redeems a link code (`Identities.CompleteLink`). `discord.Commands` registers the command for every Discord application in the background, with backoff; the start never waits for Discord. What it has registered is a memory of the process, which is fine because registering is an overwrite with the same content.
- `docs/discord.md` is the setup guide. Update it when permissions or settings change.

### Email

- `internal/email` is the email channel: SMTP with the standard library (`net/smtp`, `crypto/tls`), and a message built with `mime/multipart` and `mime/quotedprintable`. There is no mail library. `docs/email.md` is the guide; keep it true.
- An email cannot be edited, so every notification is a message. The reference a `Send` returns is the `Message-ID`, which the next message about the group answers. The identifier is made from the notification's row, so a repeated attempt repeats the identifier.
- **Never downgrade.** With `starttls`, a server that does not offer `STARTTLS` is refused for good; the certificate is verified against the system's authorities for the configured host; the password is sent with `PLAIN` or `LOGIN` only over TLS or to this machine, and validation refuses `tls: none` with a password for any other host. Do not add a setting that turns verification off.
- **Nothing from an alert reaches a header or the SMTP exchange as it is.** The subject is the only header an alert fills: it goes through `oneLine` and RFC 2047 encoding. The body is quoted-printable, the HTML form passes everything through `html.EscapeString`, and an address becomes a link only through `webAddress`. A destination must be one plain address (`recipient`), and only a `Message-ID` that Beacon made goes back into `In-Reply-To`. `TestAnAlertCannotAddHeadersOrMessages` runs the usual attacks through a real server.
- A reply that starts with 5 is `notify.Permanent`; one that starts with 4, and every failure of the network, is retried.
- The link in an email leads to the alert's page in the web interface (`AlertPath`), which asks for a sign-in.
- An email carries no link that changes anything. A link that acknowledges would be followed by mail scanners; a safe one needs a confirmation page, which belongs to the web interface.
- Tests that need a real SMTP server use `mailpittest` against the Mailpit in `compose.yaml` (`dev/mailpit/auth` holds its one account). Without `BEACON_TEST_SMTP_ADDR` and `BEACON_TEST_MAILPIT_URL` they are skipped locally and fail in CI. The server is shared by every test, so a test sends to an address of its own (`mailpittest.Address`). Use the scripted server in `internal/email/fake_test.go` only for what Mailpit will not do: TLS, `LOGIN` alone, a reply at a chosen step.

### Outbound webhook

- `internal/webhook` is the outbound webhook channel: one `POST` per notification, with the standard library. `docs/outbound-webhook.md` is the contract for the payload, the headers and the signature. **The payload is versioned**: adding a field is free, and removing one or changing its meaning needs a new `Version`. `testdata/payload.golden.json` holds version 1; a change to it is a change to the contract, and to the page. An alert's `url` is its address in the API and `web_url` its page in the web interface; neither may come to mean the other.
- Every request is signed (`Sign`: HMAC-SHA256 over `v1:<timestamp>:<body>`), and the signing secret is required by validation. `Verify` is what a receiver does; the tests use it in a receiver's place. The notification's identifier is the idempotency key, in the `Idempotency-Key` header and as the payload's `id`, and is the same in every attempt.
- The address is the operator's, and usually internal, so private addresses are allowed. The client (`newClient`) never follows a redirect, refuses link-local addresses when the connection is made, reads a bounded part of an answer, and bounds every attempt. Keep all four.
- **Nothing of the address, the headers or the answer goes into an error or a log**: the address can hold a token, and an answer can repeat one. An error names the status code. The destination of a webhook notification is the name of the integration.
- A webhook has one address and no channels: its target is `{integration: <name>}`, and validation refuses it a `channel` and refuses it in `via`.
- A `3xx` and a `410` are permanent; every other failure is retried, and `Retry-After` is honoured up to an hour.

### End-to-end tests

- `internal/e2e` builds the binary, runs it as a process against a real Postgres, the fake Slack, the fake Discord, the Mailpit of `compose.yaml` and, for outbound webhooks, an HTTP server of the test's own, and uses it only from outside: the webhook as a source would, the interactions endpoint as Slack would. It reads the database to check what was stored and never writes to it. The tests run with `go test ./...`, so CI runs them against the Postgres from `compose.yaml`.
- The test configuration sets `group_wait: 0s` and `group_interval: 1s`, so a notification follows a delivery within about two seconds. The tests wait for the message they expect, never for a time, and must not count notifications: how many updates a group gets depends on what the timer batched.
- `TestFirstPath` is the path the product exists for: webhook in, Slack message out, acknowledgement from Slack, resolve from the source. The acknowledgement needs a Beacon user, so the test signs one in through Dex first (`startSignedIn`); `TestSlackWithoutSignIn` is the same path in an install with no identity provider, where the alert can be resolved from Slack and not acknowledged. `TestFirstPathWithEscalation` is the same path with people in it: the person on call is paged, the second step tells the team and a channel, and an acknowledgement stops the escalation. `TestDiscordFirstPath` is the path through Discord: a person signs in, links their Discord account with a code, is paged in a direct message and acknowledges from it. A change that breaks any of them is not ready. Extend these tests when a feature changes that path, for example when routing decides where the message goes.
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
