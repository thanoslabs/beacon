---
title: Configuration
tags: [configuration, reference]
date: 2026-11-17
---

Beacon's provisioned objects are defined in YAML files that live in version control: sources, integrations, teams, schedules, escalation policies and routing. So is how people sign in. This page is the reference for those files. Why they look the way they do is in [[configuration-and-data-model]].

> [!warning] Loaded and checked, not all acted on yet
> Beacon reads and validates every kind described here. What it does with them is still growing: webhook sources and Slack integrations work today, and the other source and integration types, schedules, escalation and routing arrive with later releases. Each section says what is in use.

## Files

Give Beacon a path with `--config` or `BEACON_CONFIG`. The path is one YAML file, or a directory.

- A directory is read recursively. Every `*.yaml` and `*.yml` file is taken, in lexical order of its path. Files and directories whose names start with a dot are skipped.
- Every file is one YAML document with the same shape: top-level keys named after the kinds, each holding a list. A file may hold any subset of the keys, and the lists of all files are joined. A team can keep its schedule and its escalation policy together in one file.
- `routing` is the exception: its rules are ordered, so the whole block must be in one file.
- A field Beacon does not know is an error, and so is the same name used twice within a kind, in any file.

[`examples/full/beacon.yaml`](https://github.com/thanoslabs/beacon/blob/main/examples/full/beacon.yaml) is a complete configuration with every kind.

## Common rules

| Rule | Detail |
|---|---|
| Names | Every object has a `name`, unique within its kind: lowercase letters, digits and hyphens, starting with a letter, at most 63 characters. The name is the identity. Renaming an object removes the old one and creates a new one. |
| References | Objects refer to each other by `name`. Users are referred to by email address. |
| Durations | `30s`, `5m`, `12h`, `1d`, `1w`. Units may be combined from largest to smallest, as in `1h30m`. |
| Dates and times | RFC 3339 with a zone: `2026-01-05T09:00:00Z`. |
| Time zones | IANA names such as `Europe/London`. |
| Label names | Letters, digits and underscores, not starting with a digit, as in Prometheus. |

## Secrets

A secret is never written in a file. A secret field takes a reference instead:

```yaml
token: {env: BEACON_SOURCE_PROD_TOKEN}       # an environment variable of the Beacon process
token: {file: /run/secrets/prod-token}       # a file; a trailing newline is dropped
```

- A plain string in a secret field is an error. The error does not repeat what was written.
- A relative `file` path is taken from the directory of the configuration file that names it.
- Beacon reads the values when it loads the configuration. It never stores them: the copy in the database keeps the reference. It never logs or prints them.

## Sources

A source is an authenticated endpoint that receives alerts, at `/api/v1/ingest/<name>`.

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Identity, and the last segment of the URL |
| `type` | yes | `alertmanager`, `grafana` or `webhook`. Only `webhook` receives alerts today; see [[webhook]]. |
| `token` | yes | Secret that the sender presents as a bearer token, at least 16 characters |
| `labels` | no | Labels added to every alert from this source. They do not replace a label the alert already has. |
| `severity.label` | no | The label that carries severity (default `severity`) |
| `severity.mapping` | no | Maps values of that label to `critical`, `warning` or `info`. Without it, those three names map to themselves. |
| `severity.default` | no | Applies when the label is missing or unmapped (default `warning`) |

## Integrations

An integration is a connection to a notification service.

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Identity |
| `type` | yes | `slack`, `discord`, `email` or `webhook`. Only `slack` delivers today; see [[slack]]. |
| `slack`, `discord`, `email` or `webhook` | yes | The settings. Exactly the block that matches `type`. |

| Type | Settings |
|---|---|
| `slack` | `bot_token` (secret), `signing_secret` (secret) |
| `discord` | `bot_token` (secret), `public_key` (64 hexadecimal characters), `application_id` |
| `email` | `host`, `port`, `from`, `tls` (`starttls`, `implicit` or `none`), and optionally `username` with `password` (secret) |
| `webhook` | `url`, and optionally `headers`: a map whose values are plain strings or secret references |

## Teams

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Identity |
| `display_name` | no | Shown to people; defaults to `name` |
| `members` | no | User email addresses |
| `groups` | no | Identity provider group names whose members belong to the team. Applied when a person signs in; see [[access#Roles and teams]]. |

### Team membership

A person belongs to a team when they are in its `members` list **or** in one of its `groups` at the identity provider. Membership is the union of the two, and each is kept apart from the other:

| | From `members` | From `groups` |
|---|---|---|
| Set by | The list in the files | The groups in the person's ID token |
| Applied | When the configuration is applied (start and reload), and when the person signs in | When the person signs in |
| Removed by | Taking the address off the list | The person leaving the group at the provider, at their next sign-in |

Removing someone from `members` does not remove them while their group still maps to the team, and the reverse.

`members` are email addresses, compared without regard to case. **A listed address that belongs to no user is skipped**: people become users when they first sign in, so a team can list someone who has not arrived yet.

- Each skipped address is logged as an error, `team member skipped`, with the team and the address, every time the configuration is applied. Nothing else is held up: the rest of the team, and the rest of the configuration, is applied.
- The metric `beacon_team_members_skipped{team="..."}` counts them per team. A value that stays above zero usually means a misspelt address, or someone who has left.
- The person joins the team by themselves at their first sign-in. No reload is needed.
- If the identity provider changes someone's email address, the list must be changed too. Until then the old address is skipped, and the person drops out of the team at their next sign-in.

## Schedules

A schedule is one rotation.

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Identity |
| `team` | yes | The owning team |
| `time_zone` | yes | The zone in which the handoff time is read |
| `rotation.members` | yes | User email addresses, in rotation order |
| `rotation.shift_length` | yes | A whole number of days or weeks: `1d`, `3d`, `1w`, `2w` |
| `rotation.handoff.time` | yes | Local time of the handoff, quoted: `"09:00"` |
| `rotation.handoff.day` | for weeks | Weekday of the handoff, such as `monday`. Required when the shift length is whole weeks, and not allowed otherwise. |
| `effective_from` | no | When a changed definition takes over. Without it, the change applies at the next handoff. |

## Escalation policies

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Identity |
| `repeat` | no | How many more times all steps run after the first pass (default `0`, at most `100`) |
| `steps` | yes | Ordered list, at least one |
| `steps[].notify` | yes | Targets, at least one |
| `steps[].timeout` | yes | How long to wait for an acknowledgement before the next step |

A target has exactly one of these forms:

| Target | Who is notified |
|---|---|
| `{schedule: <name>}` | Whoever is on call for the schedule |
| `{team: <name>}` | Every member of the team |
| `{user: <email>}` | One user |
| `{channel: <channel>, integration: <name>}` | A shared channel, for example a Slack channel |

The first three accept `via`, a list of integration names that narrows how the person is reached.

## Routing

| Field | Required | Meaning |
|---|---|---|
| `group_by` | no | Labels that define a group (default `[alertname]`) |
| `group_wait` | no | Wait before the first notification of a new group (default `30s`) |
| `group_interval` | no | Minimum time between updates for a group (default `5m`) |
| `default.escalation_policy` | yes | Policy for alerts that match no rule |
| `default.team` | no | Team for alerts that match no rule |
| `rules` | no | Ordered list; the first matching rule wins |

| Rule field | Required | Meaning |
|---|---|---|
| `name` | yes | Identity |
| `match` | one or both | Map of label to exact value; all must match |
| `match_re` | one or both | Map of label to regular expression ([RE2 syntax](https://github.com/google/re2/wiki/Syntax)), anchored at both ends; all must match |
| `team` | no | Team the alert is assigned to |
| `service` | no | Service name the alert is assigned to |
| `escalation_policy` | yes | Policy to run |
| `group_by`, `group_wait`, `group_interval` | no | Override the routing settings for this rule |

## Sign-in

The `auth` block says how people sign in. It is one group of settings, not a list of objects, and must be in a single file. [[access]] describes it and what to set up at the identity provider.

```yaml
auth:
  public_url: https://beacon.example.com
  session_lifetime: 12h
  oidc:
    issuer: https://idp.example.com
    client_id: beacon
    client_secret: {env: BEACON_OIDC_CLIENT_SECRET}
    groups_claim: groups
  roles:
    admin: [beacon-admins]
    member: [beacon-users]
```

## How changes are applied

When Beacon starts, it makes its database match the files. Each provisioned object has a row there, so that alerts and other history have something to refer to.

| Change in the files | What happens |
|---|---|
| A new object | It is created. |
| An edited object | It is updated. Objects that did not change are left untouched. |
| A removed object | It stops working at once: a removed source refuses alerts. Its row is kept and marked as removed, so everything that referred to it still shows what it was. |
| A renamed object | The name is the identity, so this is a removal and a creation. History stays with the old name. |
| A removed object that returns under the same name | It is the same object again, with its history. |

The whole change is applied together or not at all. Applying the same files again changes nothing, and several replicas that start together do not get in each other's way. Run every replica with the same files: if two disagree, each applies its own and the last one wins.

Provisioned objects are read-only everywhere else. Nothing but the files can change them.

## Reload without a restart

Beacon reads the files when it starts, and again whenever it receives `SIGHUP`:

```sh
kill -HUP "$(pidof beacon)"
docker compose kill --signal HUP beacon
kubectl exec deploy/beacon -- kill -HUP 1
```

An admin can ask for the same over HTTP, with `POST /api/v1/config/reload` (see [[access]]). It answers `200` with `{"result": "reloaded"}`, or `422` with the list of problems when the configuration is rejected. The request reaches one replica; the signal is the way to reach each of them.

- The files are checked first, exactly as `beacon validate --check-secrets` checks them. If anything is wrong, **nothing** of the new configuration is applied and the previous one stays active. The log has one `configuration problem` record per problem, then `configuration reload failed; the previous configuration stays active`.
- A valid configuration is applied as a whole, as at start: the database is brought in line, then sources, integrations and notification targets switch over together. Requests in progress finish with what they started with. The log says `configuration reloaded`.
- Secrets kept in files are read again, so a reload also picks up a rotated secret. Secrets from environment variables cannot change while the process runs; rotating one needs a restart.
- Each replica reloads on its own. Send the signal to every replica.
- Process settings (flags and their environment variables) are not part of the configuration and need a restart.

Three metrics on `/metrics` follow reloads:

| Metric | Meaning |
|---|---|
| `beacon_config_reloads_total{result="success"}`, `{result="failure"}` | Reloads since the process started. Loading at start is not counted. |
| `beacon_config_last_reload_successful` | `1` after a successful load, `0` after a rejected reload. While it is `0`, the process runs a configuration that differs from the files. |
| `beacon_config_last_reload_success_timestamp_seconds` | When the active configuration was loaded |

An alert on `beacon_config_last_reload_successful == 0` catches a broken configuration that was rolled out without `beacon validate`.

## Check the configuration

`beacon validate` checks the files without applying them. It needs no database and no secrets, so it can run in CI on every change:

```sh
beacon validate --config beacon.d
```

```
the configuration at beacon.d is valid: 2 sources, 4 integrations, 1 team, 1 schedule, 1 escalation policy, 1 routing rule
```

When something is wrong, every problem is reported in one run, one per line, with the file, line, object and field at fault. The command exits with status 1:

```
beacon.d/platform.yaml:12: schedule "platform-primary": team: team "plataform" does not exist
beacon.d/platform.yaml:19: schedule "platform-primary": rotation.handoff.day: is required when shifts are whole weeks: a weekday such as monday
beacon.d/sources.yaml:4: source "prod": token: secrets are never written inline; use {env: NAME} or {file: /path}
beacon validate: the configuration at beacon.d is not valid: 3 problems
```

It checks the YAML itself, unknown fields, the type and allowed values of every field, and every reference from one object to another. It cannot check that a user exists, because users live in the database.

By default no secret is read: a reference only has to be well formed. Where the secrets are present, `--check-secrets` also reads each one and fails when a variable or a file is missing.

With the container image:

```sh
docker run --rm -v "$PWD/beacon.d:/etc/beacon:ro" ghcr.io/thanoslabs/beacon:X.Y.Z validate --config /etc/beacon
```
