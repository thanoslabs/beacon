---
title: Sign-in and access
tags: [access, guide]
date: 2026-11-17
---

People sign in to Beacon through your organisation's identity provider, with OpenID Connect. A user is created in Beacon's database the first time they sign in, and updated on every later sign-in. Automation authenticates with [API tokens](#api-tokens). An install that has no identity provider yet starts with the [bootstrap admin](#bootstrap-admin).

People use Beacon in a browser through the [[web|web interface]], which shows the sign-in page to anyone who is not signed in. After signing in, the browser lands on the page that was asked for.

## Set up the identity provider

Register Beacon with your provider as a **web application** (a confidential client) that uses the **authorisation code** flow.

| Setting at the provider | Value |
|---|---|
| Redirect URI | Beacon's public address followed by `/auth/callback`, for example `https://beacon.example.com/auth/callback` |
| Scopes | `openid`, `profile`, `email`, and whatever scope makes the provider put groups or roles into the ID token (often `groups`) |
| Grant type | Authorisation code. Beacon uses PKCE (`S256`) as well as the client secret. |

Note the **issuer URL**, the **client ID** and the **client secret** that the provider gives you. The issuer URL is the one under which `/.well-known/openid-configuration` is served.

## Configure Beacon

Sign-in is part of the configuration files (see [[configuration]]):

```yaml
auth:
  public_url: https://beacon.example.com
  session_lifetime: 12h
  oidc:
    issuer: https://idp.example.com
    client_id: beacon
    client_secret: {env: BEACON_OIDC_CLIENT_SECRET}
```

| Field | Required | Meaning |
|---|---|---|
| `public_url` | yes | The address at which browsers reach Beacon. It must be `https`; plain `http` is accepted only for `localhost`. |
| `session_lifetime` | no | How long a sign-in lasts, from `1m` to `30d` (default `12h`) |
| `oidc.issuer` | yes | The provider's issuer URL. It must be `https`, except for a provider on `localhost`. |
| `oidc.client_id` | yes | The client ID |
| `oidc.client_secret` | yes | The client secret, as a secret reference |
| `oidc.scopes` | no | Scopes to request (default `openid`, `profile`, `email`). The list must include `openid`. |

The `auth` block is one group of settings and must be in a single file. Without it, nobody can sign in. It is applied on a [[configuration#Reload without a restart|reload]] like everything else.

Beacon reads the provider's discovery document when it starts. If the provider cannot be reached, Beacon starts anyway, logs a warning, and tries again when someone signs in: alerts keep flowing while the provider is down.

## Who may use what

Beacon is closed by default. Every route needs a session unless it is on the short list of public routes below, and a request to anything else under `/api/` and `/auth/`, including an address that does not exist, is answered with `401`. Every other address is a page of the web interface, which is public and shows nothing but the sign-in page until someone has signed in.

| Endpoint | Who | Purpose |
|---|---|---|
| `GET /auth/login` | Public | Starts a sign-in and sends the browser to the provider |
| `GET /auth/callback` | Public | Where the provider sends the browser back. This is the redirect URI. |
| `GET /auth/methods` | Public | Which ways to sign in exist, for a sign-in page: `{"oidc": true, "bootstrap_admin": false}` |
| `POST /auth/logout` | Member | Ends the session. Answers `204`. |
| `GET /api/v1/me` | Member | Who is signed in: `id`, `email`, `display_name`, `role` and `session_expires_at` |
| `GET /api/v1/me/identities` | Member | The chat accounts linked to whoever is signed in; see [Chat accounts](#chat-accounts) |
| `POST /api/v1/config/reload` | Admin | Reloads the configuration; see [[configuration#Reload without a restart]] |
| `GET /api/v1/tokens`, `POST /api/v1/tokens`, `DELETE /api/v1/tokens/<id>` | Admin | Lists, creates and revokes [API tokens](#api-tokens) |
| The routes of alerts and alert groups | Member | Lists, reads and acts on alerts; see [[api#Alerts]] |
| `GET` on sources, integrations, escalation policies, routing, teams and users | Member | What the configuration defines, for reading, and the users; see [[api#What the configuration defines]] |
| `GET /api/v1/silences`, `POST /api/v1/silences`, `POST /api/v1/silences/preview`, `GET /api/v1/silences/<id>`, `DELETE /api/v1/silences/<id>` | Member | Lists, creates, previews, reads and expires [[silences]] |
| `GET /api/v1/schedules`, `GET /api/v1/schedules/<name>`, `GET /api/v1/schedules/<name>/shifts`, `GET /api/v1/teams/<name>/on-call` | Member | Schedules, who is on call now and over a period; see [[schedules#Who is on call]] |
| `GET /api/v1/schedules/<name>/overrides`, `POST /api/v1/schedules/<name>/overrides`, `DELETE /api/v1/schedules/<name>/overrides/<id>` | Member | Lists, creates and removes the overrides of a schedule; see [[schedules#Overrides]] |
| `POST /auth/bootstrap` | Public | Signs the [bootstrap admin](#bootstrap-admin) in. The password is checked and the attempts are bounded; `404` unless the account is enabled. |
| `POST /api/v1/ingest/<source>` | Public | Receives alerts. The source's own token is checked; see [[webhook]]. |
| `POST /api/v1/integrations/<name>/interactions` | Public | Receives button presses from Slack and from Discord. The service's signature is checked; see [[slack]] and [[discord]]. |
| `GET /api/v1/openapi.json`, `GET /api/v1/openapi.yaml` | Public | The OpenAPI description of the API; see [[api#OpenAPI description]] |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | Public | Probes and metrics, for machines that have no session |
| `GET` on every other address that is not under `/api/` or `/auth/` | Public | The [[web|web interface]]: one page with its script and style sheet, the same for everyone. It holds nothing of an install; what it shows it asks of the routes above. |

"Member" means anyone who is signed in or presents an API token, with either role. "Admin" needs the admin role; a member gets `403`.

> [!warning] Keep `/metrics` off the public internet
> The probes and the metrics need no session, because the systems that call them have none. Metrics name your teams. If Beacon is reachable from outside, let your proxy expose only what outsiders need: `/api/v1/ingest/` and `/api/v1/integrations/` for the systems that send to Beacon, and, if people use the web interface from outside, everything except `/metrics`, `/healthz` and `/readyz`.

## API tokens

A token lets a script or another system use the API. It carries a role of its own, `admin` or `member`, and is sent as a bearer token:

```sh
curl -H "Authorization: Bearer $BEACON_TOKEN" https://beacon.example.com/api/v1/me
```

Only an admin can manage tokens, because a token can be given any role.

```sh
# Create. The token is in the answer, and is shown this once.
curl -X POST https://beacon.example.com/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name": "deploy-pipeline", "role": "member", "expires_in": "90d"}'

# List. Shows names, roles and times, never the tokens. Paged with limit and cursor.
curl -H "Authorization: Bearer $ADMIN_TOKEN" https://beacon.example.com/api/v1/tokens

# Revoke. The token stops working at once.
curl -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" https://beacon.example.com/api/v1/tokens/<id>
```

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | What the token is for, up to 100 characters |
| `role` | yes | `admin` or `member` |
| `expires_in` | no | How long the token is valid, such as `12h`, `30d` or `52w`, up to five years. Without it the token does not expire. |

- A token starts with `bcn_`, so that one can be recognised if it ends up where it should not be.
- Beacon stores only a hash of each token. A token that is lost cannot be read again; revoke it and create another.
- A token is not tied to the person who created it. It keeps working when they leave, and when sign-in is reconfigured or removed. Review the list from time to time; it shows when each token was last used, to the minute.
- A request that carries a token is judged by the token alone, even if the browser also has a session.

The first token has to be created by an admin who is signed in: through the identity provider, or as the bootstrap admin.

## Bootstrap admin

A new install has no admin: nobody has signed in, and there may be no identity provider yet. The bootstrap admin is one built-in admin account with a password, for that time. **Turn it off once an admin can sign in through the identity provider.**

1. Hash a password. It must be at least 12 characters; make it long and random. The command asks for it twice without showing it, or reads it from a pipe:

   ```sh
   beacon hash-password
   docker run --rm -it ghcr.io/thanoslabs/beacon:X.Y.Z hash-password
   ```

2. Give Beacon the hash as a secret, and enable the account:

   ```yaml
   auth:
     public_url: https://beacon.example.com
     bootstrap_admin:
       password_hash: {env: BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH}
   ```

   In a shell or a Compose file, mind the `$` signs in the hash: quote it with single quotes, and in `compose.yaml` write each `$` as `$$`. A file (`{file: ...}`) avoids the question.

3. Sign in, and use the session cookie like any other:

   ```sh
   curl -c cookies.txt -X POST https://beacon.example.com/auth/bootstrap \
     -H 'Content-Type: application/json' -d '{"password": "..."}'
   curl -b cookies.txt https://beacon.example.com/api/v1/me
   ```

What to know about it:

- Beacon is given only the bcrypt hash and never stores the password. The hash is treated as a secret all the same: it is never logged or printed.
- Sign-in is bounded to five attempts a minute for each Beacon process, counting every attempt. Further attempts get `429` until the minute is over, including one with the right password. This slows guessing down; it also means that someone hammering the address can keep you out while they do, which is one more reason to turn the account off.
- It has the admin role and no identity at any provider. It appears as the user `bootstrap-admin@beacon.invalid`.
- **To turn it off**, remove `bootstrap_admin` from the configuration and reload. Its sessions end at once, on every replica, and `POST /auth/bootstrap` answers `404`. API tokens it created keep working.
- It can be enabled together with an identity provider, for the hand-over: sign in as the bootstrap admin, set `roles.admin`, check that an admin can sign in through the provider, then remove the account. Beacon logs a warning at every start and reload while both are enabled.

## Users

- A user is identified by the provider's issuer and subject, which never change. Email address and display name are copied from the ID token at every sign-in.
- Beacon needs the `email` claim. If the provider says the address is not verified (`email_verified: false`), the sign-in is refused: configuration refers to users by email address, so an address that anyone could claim must not be trusted.
- If a person's email address changes at the provider, they keep their Beacon account and its history, and the address is updated at their next sign-in. Configuration files that list the old address must be changed to the new one.
- A sign-in is refused when its email address already belongs to another Beacon user, for example after moving to a different provider. The log names the address. Nothing is merged automatically.

## Chat accounts

Beacon keeps track of which account in Slack or Discord belongs to which user. It needs that in two directions: to know who a person acting from chat is, and to reach a person there.

| Service | How an account is linked |
|---|---|
| Slack | Beacon finds it by the user's email address, without the person doing anything; see [[slack#People]] |
| Discord | The person links it themselves, with a one-time code; see [[discord#People]] |

- **Acknowledging and snoozing from chat need a linked account.** A chat account that belongs to no Beacon user can resolve an alert and nothing else ([[slack#Who can act]], [[discord#Who can act]]). An install without an identity provider has no users to link, so nobody can acknowledge from chat there.
- An account belongs to one user. Beacon never moves an account from one person to another: if Slack reports, for one user's address, an account that another user has linked, it is logged as an error and nothing changes, and a Discord account that is someone else's cannot be linked with a code.
- An identity is tied to the integration through which it is known. A user ID of Slack means something in one workspace only, so a person has one identity per Slack integration.
- The bootstrap admin is an account, not a person with an address, and is never looked up.
- A disabled user's accounts are attributed to nobody.

`GET /api/v1/me/identities` answers the accounts of whoever asks:

```json
{"identities": [
  {"integration": "slack", "provider": "slack", "external_id": "U0123ABCDEF", "origin": "email", "linked_at": "2026-12-01T10:12:00Z"}
]}
```

`origin` is `email` for an account that Beacon found by the email address and `self` for one the person linked. An API token is not a person and has no identities.

For a service in which people link their accounts themselves, two more routes act on the account of whoever asks. `<integration>` is the name of the integration:

| Route | What it does |
|---|---|
| `POST /api/v1/me/identities/<integration>/link-code` | Issues a one-time code, valid for ten minutes, and answers `201` with it and with the command to type in the service. [[discord#People]] shows the whole exchange. |
| `DELETE /api/v1/me/identities/<integration>` | Removes the account the person linked. Answers `204`, or `404` when there is none. |

Both answer `404` for a name that is no chat integration, `409` for an integration whose accounts are linked by email address, such as Slack, and `403` for an API token. The bootstrap admin gets `403` for a code: it is not a person.

## Roles and teams

Beacon has two roles. An `admin` may do everything; a `member` may view and act on alerts and other runtime state. Both come from the identity provider: Beacon reads the person's roles or groups from one claim of the ID token and maps them.

```yaml
auth:
  oidc:
    # ...
    scopes: [openid, profile, email, groups]
    groups_claim: groups
  roles:
    admin: [beacon-admins]
    member: [beacon-users]

teams:
  - name: platform
    groups: [platform-engineers]
```

| Field | Meaning |
|---|---|
| `oidc.groups_claim` | The claim that carries the person's roles or groups (default `groups`) |
| `roles.admin` | Values of that claim whose holders are admins |
| `roles.member` | Values whose holders are members. When the list is empty or left out, everyone who can sign in is a member. |
| `teams[].groups` | Values whose holders belong to the team. See [[configuration#Teams]]. |

How a person's role is decided, at every sign-in:

1. In any `roles.admin` group: **admin**.
2. Otherwise, in any `roles.member` group, or `roles.member` is empty: **member**.
3. Otherwise: **no access**. The sign-in is refused with `403`.

Without a `roles` block everyone who can sign in is a member and nobody is an admin.

**The mapping is applied again at every sign-in**, from the groups in that sign-in's token:

- The role is set to what the groups give now. An admin who left the admin group is a member from their next sign-in.
- Team memberships that came from groups are replaced with the teams whose `groups` match now. Memberships from a team's `members` list are not touched; see [[configuration#Team membership]].
- A person who is no longer in any mapped group is refused, and what earlier sign-ins gave them is taken back at that moment: their sessions end and their group memberships are removed. Their user stays, for the history that refers to it.

> [!warning] Changes at the provider take effect at the next sign-in
> Beacon learns about groups only when a person signs in. Someone removed from a group at the provider keeps their role and teams until their session ends, at most `session_lifetime` (12 hours by default). Shorten `session_lifetime` to tighten this. SCIM provisioning, which would remove the delay, is a later requirement.

### The claim

- The claim's value may be a list of strings or a single string. Names are compared exactly, including case.
- A claim inside an object is named by its path, with dots: Keycloak's realm roles are `realm_access.roles`. A claim whose own name contains dots, such as a URL, is found by its whole name first.
- The claim must be **in the ID token**. Many providers add it only when a scope is requested (Dex and Okta: `groups`) or when a mapper or an optional claim is configured (Keycloak, Entra ID). If nobody can sign in after you add `roles`, check that the claim is in the token: Beacon logs `sign-in refused` with the number of groups it found.

## Sessions

- A sign-in, through the provider or as the bootstrap admin, gives the browser a session cookie. It is `HttpOnly`, `SameSite=Lax` and, whenever `public_url` is `https`, `Secure` with the `__Host-` prefix.
- A session ends at sign-out or after `session_lifetime`, whichever comes first. It is not extended by use. There is no refresh: when it ends, the person signs in again, which is also when their details are read from the provider again.
- Sessions are stored in Postgres, as a hash of the cookie's value, so every replica accepts them and reading the database does not let anyone take one over.
- Requests that change something are refused when another site made the browser send them (`Sec-Fetch-Site` and `Origin` are checked).

## What is checked at sign-in

Beacon uses the authorisation code flow with `state`, `nonce` and PKCE:

- the callback is accepted only in the browser that started the sign-in, once, within ten minutes;
- the ID token's signature is verified against the provider's published keys, and its issuer, audience (the client ID) and expiry are checked;
- the `nonce` in the ID token must be the one that this sign-in sent.

## Try it locally

`compose.yaml` includes [Dex](https://dexidp.io/), a small identity provider, set up with a few users whose password is `password` (see `dev/dex/config.yaml`):

```sh
docker compose up -d --wait postgres dex
cat > /tmp/beacon-auth.yaml <<'YAML'
auth:
  public_url: http://127.0.0.1:18080
  oidc:
    issuer: http://127.0.0.1:5556/dex
    client_id: beacon
    client_secret: {env: BEACON_OIDC_CLIENT_SECRET}
YAML
export BEACON_OIDC_CLIENT_SECRET=beacon-local-development-client-secret
export BEACON_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
beacon serve --config /tmp/beacon-auth.yaml --listen-address 127.0.0.1:18080
```

Open <http://127.0.0.1:18080/auth/login>, sign in as `ada@example.com` with `password`, and the browser shows who you are. Use `127.0.0.1` and not `localhost` in the address: Dex only sends people back to the address it knows, and cookies are kept per host name.

## Tested with

Sign-in is tested in CI against Dex 2.46, a real provider, with its password connector: a first and a later sign-in, sign-out, an expired session, a wrong password and an unverified address. Roles and teams are tested with the groups Dex puts in its ID token: a `groups` claim holding a list of names, sent when the `groups` scope is requested. A hand-written provider in the tests covers what a real one will not do, such as ID tokens with a wrong issuer, audience, signature, expiry or nonce.

Not tested: any hosted provider (Okta, Entra ID, Google, Auth0, Keycloak). Beacon uses only standard discovery, the authorisation code flow and the ID token, so they are expected to work, but nobody has tried. Providers that do not publish a discovery document, or that return the email address only from the UserInfo endpoint and not in the ID token, will not work yet.

Group shapes that are covered only by tests with hand-written tokens, not by a real provider: a single string instead of a list, a nested claim such as `realm_access.roles`, and a claim named by a URL. Not supported: groups that are not in the ID token and need a separate call (the UserInfo endpoint, or Entra ID's "groups overage" for people in more than 200 groups, which points to Microsoft Graph); groups sent as objects or as one comma-separated string; and Entra ID's group object IDs work only if you write the IDs, not the names, in the configuration. If yours does not work, please open an issue.
