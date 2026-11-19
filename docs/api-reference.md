---
title: API reference
tags: [api, reference]
date: 2026-11-19
---

Every route of Beacon's HTTP API, with who may call it. This page is generated from the OpenAPI description of the API, which a running Beacon serves at `/api/v1/openapi.json` and `/api/v1/openapi.yaml` and which is published beside this page as [openapi.yaml](openapi.yaml). The description holds the parameters and the shape of every answer; [[api]] describes how the API behaves as a whole.

Do not edit this page. Change `internal/api/openapi.yaml`, then run `go test ./internal/api -run TestPublishedDescription -update`.

"Public" needs no credential, "member" a session or an API token with either role, and "admin" the admin role.

## Alerts

| Route | Who | What it does |
|---|---|---|
| `GET /api/v1/alert-groups/<id>` | Member | Read an alert group with its alerts |
| `POST /api/v1/alert-groups/<id>/acknowledge` | Member | Acknowledge every alert of a group that can be |
| `POST /api/v1/alert-groups/<id>/resolve` | Member | Resolve every alert of a group by hand |
| `POST /api/v1/alert-groups/<id>/snooze` | Member | Snooze every alert of a group that can be |
| `GET /api/v1/alerts` | Member | List alerts, newest first |
| `GET /api/v1/alerts/<id>` | Member | Read one alert |
| `POST /api/v1/alerts/<id>/acknowledge` | Member | Acknowledge an alert |
| `POST /api/v1/alerts/<id>/resolve` | Member | Resolve an alert by hand |
| `POST /api/v1/alerts/<id>/snooze` | Member | Snooze an alert |
| `GET /api/v1/alerts/<id>/timeline` | Member | What happened to an alert and its group, oldest first |

## Silences

| Route | Who | What it does |
|---|---|---|
| `GET /api/v1/silences` | Member | List silences |
| `POST /api/v1/silences` | Member | Create a silence |
| `POST /api/v1/silences/preview` | Member | Which open alerts a silence would match, without creating it |
| `DELETE /api/v1/silences/<id>` | Member | Expire a silence now |
| `GET /api/v1/silences/<id>` | Member | Read one silence |

## Schedules

| Route | Who | What it does |
|---|---|---|
| `GET /api/v1/schedules` | Member | List the schedules, with who is on call |
| `GET /api/v1/schedules/<name>` | Member | Read one schedule, with who is on call |
| `GET /api/v1/schedules/<name>/overrides` | Member | List the overrides of a schedule |
| `POST /api/v1/schedules/<name>/overrides` | Member | Create an override: someone covers the schedule for a period |
| `DELETE /api/v1/schedules/<name>/overrides/<id>` | Member | Remove an override |
| `GET /api/v1/schedules/<name>/shifts` | Member | Who is on call over a period: recorded shifts and planned ones |
| `GET /api/v1/teams/<name>/on-call` | Member | Who is on call for each schedule of a team |

## Configuration

| Route | Who | What it does |
|---|---|---|
| `POST /api/v1/config/reload` | Admin | Reload the configuration files |
| `GET /api/v1/escalation-policies` | Member | List the escalation policies |
| `GET /api/v1/escalation-policies/<name>` | Member | Read one escalation policy |
| `GET /api/v1/integrations` | Member | List the integrations, without credentials |
| `GET /api/v1/integrations/<name>` | Member | Read one integration, without credentials |
| `GET /api/v1/routing` | Member | Read the routing block: grouping, the default route and the rules in order |
| `GET /api/v1/sources` | Member | List the sources |
| `GET /api/v1/sources/<name>` | Member | Read one source |
| `GET /api/v1/teams` | Member | List the teams with their members |
| `GET /api/v1/teams/<name>` | Member | Read one team with its members |
| `GET /api/v1/users` | Member | List the users, by email address |

## Access

| Route | Who | What it does |
|---|---|---|
| `GET /api/v1/me` | Member | Who is signed in |
| `GET /api/v1/me/identities` | Member | The chat accounts linked to whoever is signed in |
| `DELETE /api/v1/me/identities/<integration>` | Member | Remove the chat account the person linked themselves |
| `POST /api/v1/me/identities/<integration>/link-code` | Member | Issue a one-time code with which the person links their chat account |
| `GET /api/v1/tokens` | Admin | List API tokens |
| `POST /api/v1/tokens` | Admin | Create an API token |
| `DELETE /api/v1/tokens/<id>` | Admin | Revoke an API token |
| `POST /auth/bootstrap` | Public | Sign the bootstrap admin in with its password |
| `GET /auth/callback` | Public | Where the identity provider sends the browser back |
| `GET /auth/login` | Public | Start a sign-in through the identity provider |
| `POST /auth/logout` | Member | End the session |
| `GET /auth/methods` | Public | Which ways to sign in exist |

## For other systems

| Route | Who | What it does |
|---|---|---|
| `POST /api/v1/ingest/<source>` | Public | Receive alerts from a source |
| `POST /api/v1/integrations/<name>/interactions` | Public | Receive button presses and commands from Slack or Discord |
| `GET /api/v1/openapi.json` | Public | This description, as JSON |
| `GET /api/v1/openapi.yaml` | Public | This description, as YAML |
| `GET /healthz` | Public | Liveness: the process is running |
| `GET /metrics` | Public | Prometheus metrics |
| `GET /readyz` | Public | Readiness: the database answers and the server is not shutting down |
