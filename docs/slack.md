---
title: Slack
tags: [notifications, slack, guide]
date: 2026-11-16
---

Beacon posts each alert group to a Slack channel as one message with buttons to acknowledge, snooze and resolve it, and pages people in a direct message that looks the same. It edits a message in place when the alert changes, so every destination shows the current state. Delivery, retries and tracking are described in [[notifications]].

> [!warning] Not yet verified against Slack itself
> This integration was built from Slack's API documentation and tested against a fake Slack that checks requests against the documented shapes and limits. Nobody has run it against a real workspace yet. If something below does not match what Slack shows you, please open an issue.

## Set up the Slack app

You need permission to add apps to your workspace.

1. Open <https://api.slack.com/apps?new_app=1> and choose to create the app **from a manifest**. Pick your workspace, and paste this manifest:

   ```yaml
   display_information:
     name: Beacon
   features:
     app_home:
       home_tab_enabled: false
       messages_tab_enabled: true
       messages_tab_read_only_enabled: true
     bot_user:
       display_name: Beacon
       always_online: true
   oauth_config:
     scopes:
       bot:
         - chat:write
         - chat:write.public
         - users:read
         - users:read.email
   settings:
     interactivity:
       is_enabled: true
       request_url: https://beacon.example.com/api/v1/integrations/slack/interactions
   ```

   Replace `beacon.example.com` with the address at which Slack can reach your Beacon. See [Buttons](#buttons) below.

2. **Install the app** to your workspace from the app's settings and approve the permissions it asks for.
3. Copy the **Bot User OAuth Token** from the app's *OAuth & Permissions* page. It starts with `xoxb-`. This is `BEACON_SLACK_BOT_TOKEN`.
4. Copy the **Signing Secret** from *App Credentials* on the app's *Basic Information* page. This is `BEACON_SLACK_SIGNING_SECRET`.
5. Choose the channel for alerts and find its ID: open the channel's details in Slack, and the ID (like `C0123456789`) is at the bottom of the *About* tab. This is the `channel` in the configuration below.
6. If the channel is private, invite the app to it: type `/invite @Beacon` in the channel.

Slack renames the pages and buttons of its settings from time to time, so the names in steps 2 to 6 may differ from what you see.

### Scopes

| Scope | Why |
|---|---|
| `chat:write` | Post messages to channels and, as direct messages, to people; and edit the messages Beacon posted |
| `chat:write.public` | Post to public channels without being invited first. Leave it out if you prefer to invite the app to each channel. |
| `users:read` | Ask what a member is called, to name someone who resolves an alert and is not a Beacon user. Without it, their Slack handle is shown. |
| `users:read.email` | Find the Slack account of a Beacon user by their email address; see [People](#people). Without it nobody is linked, and then **nobody can acknowledge or snooze from Slack**. |

Beacon asks for nothing else: it does not read messages or channels, and of users it asks two questions, which account has a given email address and what a given account is called.

## Configure Beacon

A Slack integration is defined in the configuration files (see [[configuration]]). The two credentials are never written there: the file names the environment variables, or files, that hold them.

```yaml
integrations:
  - name: slack
    type: slack
    slack:
      bot_token: {env: BEACON_SLACK_BOT_TOKEN}
      signing_secret: {env: BEACON_SLACK_SIGNING_SECRET}

escalation_policies:
  - name: default
    steps:
      - notify:
          - channel: C0123456789
            integration: slack
        timeout: 5m

routing:
  default:
    escalation_policy: default
```

| Field | Meaning |
|---|---|
| `bot_token` | The bot token from step 3 |
| `signing_secret` | The signing secret from step 4, with which Beacon checks that a button press really comes from Slack |
| `channel` | The channel from step 5. Use the channel ID. The name of a public channel also works, with or without `#`, but breaks when the channel is renamed. |

The name of the integration is part of the address that Slack sends button presses to: `/api/v1/integrations/<name>/interactions`.

> [!note] Which alerts go where
> An alert group is posted to the channels named in the **first step** of its escalation policy. With the configuration above there is one policy, the default, so every group goes to that channel. Routing rules send alerts to other policies, and so to other channels: see [[configuration#How an alert is routed]]. A policy can also page people and go on to further steps: see [[escalation]].

> [!warning] This configuration cannot acknowledge
> It has no `auth` block, so nobody can sign in and Beacon has no users. **Acknowledging and snoozing from Slack need a Beacon user whose Slack account is linked**, so with this configuration the alert arrives in Slack and can be resolved there, and the *Acknowledge* and *Snooze* buttons answer with a note. To acknowledge from Slack, add sign-in through your identity provider ([[access]]) and have each person sign in to Beacon once, with the same email address as in Slack. See [Who can act](#who-can-act).

Related alerts share one message: by default, alerts with the same `alertname`. The message of a new group is posted after `group_wait`, 30 seconds unless the `routing` block says otherwise; see [[configuration#Grouping]].

[`examples/slack`](https://github.com/thanoslabs/beacon/tree/main/examples/slack) holds this configuration with a webhook source. Copy it, put in your channel ID, and start Beacon:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
export BEACON_SLACK_BOT_TOKEN='xoxb-...'
export BEACON_SLACK_SIGNING_SECRET='...'
beacon serve --config ./my-config
```

`--slack-api-url` (`BEACON_SLACK_API_URL`) is a process setting, not part of the integration. Leave it alone; it exists to test against a fake Slack.

Then send an alert as described in [[webhook]], and a message appears in the channel.

## The message

A message is about one alert group and is edited in place as the group changes. For a group of one alert it shows:

- the state (firing, acknowledged, snoozed or resolved) and the severity;
- the `summary` annotation, or the `alertname` label when there is none;
- the `description` annotation, when present;
- a **Runbook** link, when the `runbook_url` annotation holds an `http` or `https` address;
- the labels;
- the links the source sent with the alert, when they are `http` or `https` addresses: **Source** for where the alert was generated, which Alertmanager and Grafana send, and **Dashboard**, **Panel** and **Silence at the source**, which Grafana sends;
- what last happened: since when it fires, or who acknowledged, snoozed or resolved it and when, shown in each reader's time zone.

For a group of several alerts it shows instead:

- the state and the severity of the group: firing while any alert fires, then acknowledged, then snoozed, and resolved once all are; the highest severity among the alerts that are not resolved;
- the label values the alerts share, and how many alerts are in which state;
- a list of the alerts, those that fire first, each with its `summary` and the labels that tell it apart. Ten are listed and the rest are counted;
- what last happened, as above.

Under that, once the group's escalation policy has paged people, a line says who: **Paged:** followed by everyone the policy has paged so far ([[escalation]]).

- A person whose Slack account is linked is mentioned, so the name is the one your workspace knows and leads to them. Slack notifies a mentioned person when a message is first posted and not when it is edited.
- A person who was to be paged and has no linked account ([People](#people)) is named in plain text, followed by **(Slack not connected)**, for example `Grace Hopper (Slack not connected)`. They were not notified. Whoever reads the message can see that and tell them.
- A group that has only told channels has no such line.

Alerts that a silence keeps quiet are left out of the message for as long as the silence covers them; see [[silences]].

**When Beacon asks again, it says the alert again.** An edit to a message notifies nobody in Slack. So when a step of an escalation policy reaches a channel or a person that already has the group's message, because the policy repeats or because a snooze ran out ([[escalation]]), Beacon posts a new message and keeps that one up to date from then on. The old message is turned into a one-line pointer, "Beacon sent this alert again", without buttons.

Text from an alert is escaped, so an alert cannot add links or mentions such as `@channel` to the message.

## Buttons

A message carries a button for each thing that can be done to the alert in its current state:

| Button | Effect | Offered when the alert is |
|---|---|---|
| **Acknowledge** | Marks the alert acknowledged | firing or snoozed |
| **Snooze 1h** | Snoozes it for one hour, after which it fires again | firing or acknowledged |
| **Resolve** | Resolves it | not yet resolved |

A press acts on the whole group: every alert of the group whose state allows the action takes it, and a button is offered when at least one alert does. The message then changes to show the new state, who acted and when, and the buttons that still apply. The action is recorded on the alert's timeline with the Slack user who pressed the button (see [[alert-lifecycle]]).

If the alert has changed in the meantime, for example because someone else acknowledged it a moment earlier, nothing happens and Slack shows a note that only you can see, such as "Nothing changed: this alert is already acknowledged."

### Who can act

| The person who presses | Acknowledge | Snooze | Resolve |
|---|---|---|---|
| has a Slack account linked to a Beacon user ([People](#people)) | yes | yes | yes |
| is anyone else who can see the message | no | no | yes |

- **Acknowledging and snoozing need a Beacon user.** They say "I have this", and that only means something from a person Beacon knows: the acknowledgement is recorded as theirs, and it stops the escalation.
- **Anyone who can see the message may resolve.** Resolving states a fact about the alert, and the person who fixed the problem may be a colleague who has never signed in to Beacon.
- **A refused press changes nothing** and is answered with a note that only that person sees: *Nothing changed: only Beacon users can acknowledge an alert, and your Slack account is not connected to one. Sign in to Beacon at https://beacon.example.com with the same email address as your Slack account, and Beacon connects the two within a minute. You can still resolve the alert from here.*
- **To be able to acknowledge**, a person needs three things: Beacon must have sign-in through an identity provider configured ([[access]]); they must have signed in to Beacon once; and their email address there must be the one of their Slack account. Beacon then links the two by itself. The bootstrap admin is not a person with an address and cannot acknowledge from Slack.
- **Someone who is not a Beacon user and resolves** is named as your workspace names them, with a note: the message reads *Resolved by Gus Guest (Slack not connected)*, and the timeline records that name with their Slack account. Beacon asks Slack for the name (`users.info`); if that fails or takes too long, the resolve happens all the same under their Slack handle.
- If Beacon cannot check whether an account is linked, because its database does not answer, it does not guess: acknowledging and snoozing answer *Beacon could not check who you are just now*, and resolving works.

### Request URL

When a button is pressed, Slack sends a request to the app's interactivity **Request URL**. Set it to:

```
https://<your Beacon address>/api/v1/integrations/slack/interactions
```

`slack` in the path is the name of the integration. The manifest above sets the URL; to change it later, open *Interactivity & Shortcuts* in the app's settings.

- Slack must be able to reach this address from the internet, over `https`. Only this one path needs to be reachable from outside. To try Beacon from a laptop, a tunnelling tool that gives you a public `https` address works.
- Beacon accepts a request on this path only when it carries a valid signature made with the signing secret and is less than five minutes old. Anything else gets `401` and changes nothing.
- Slack waits three seconds for the answer. If Beacon is down or slow, Slack shows the person an error next to the button.

## People

Beacon links each of its users to their account in your workspace, so that it knows who a person in Slack is.

- **By email address.** For every user who has signed in to Beacon ([[access]]), Beacon asks Slack which member has that email address (`users.lookupByEmail`), and stores the answer. It asks shortly after the person's first sign-in, and when a Slack integration is added.
- **Someone Slack does not know** is logged as a warning, `no account in the chat service has this user's email address`, and counted in the metric `beacon_users_without_identity{integration="..."}`. Beacon asks again every hour, and at once when the person's address in Beacon changes. A member whom Slack has deactivated is not found either.
- **The addresses must be the same** in the identity provider and in Slack. Nothing else is tried: a name or a similar address is not a match.
- **An action in Slack is attributed to the linked user.** When someone whose account is linked presses a button, the timeline records the Beacon user together with the Slack account, the alert names them as the person who acknowledged it, and the message shows their name in Beacon. An account that is not linked can resolve an alert and nothing else; see [Who can act](#who-can-act).
- A person sees their linked accounts at `GET /api/v1/me/identities`.
- **A person is paged in a direct message.** When a step of an escalation policy names a schedule, a team or a user ([[escalation#Targets]]), each person gets the group's message from the app, in the app's *Messages* tab, with the same buttons as in a channel. The manifest above turns that tab on (`messages_tab_enabled`); without it Slack refuses with `messages_tab_disabled`. A person whose account is not linked cannot be paged: the step skips them, and its event on the timeline says so.

## When something goes wrong

Every attempt is recorded on the alert's timeline with Slack's error. The common ones:

| Error | Meaning | What Beacon does |
|---|---|---|
| `not_in_channel` | The app is not in the channel | Gives up at once. Invite the app, or add `chat:write.public`. |
| `channel_not_found` | The channel does not exist, or is private and the app is not in it | Gives up at once |
| `invalid_auth`, `token_revoked`, `account_inactive` | The bot token is wrong or no longer valid | Gives up at once |
| `missing_scope` | The app lacks `chat:write`. For finding people by email address it is `users:read.email`: that failure is in Beacon's log as `link identities`, and nobody is linked until the scope is added and the app reinstalled. | Gives up at once |
| `messages_tab_disabled` | A person was to be paged, and the app's *Messages* tab is off | Gives up at once. Turn it on under *App Home* in the app's settings. |
| Rate limited (HTTP 429) | Too many messages | Retries, no sooner than Slack asks |
| HTTP 5xx, timeouts, `internal_error` and codes Beacon does not know | Slack is having trouble | Retries with backoff, up to the limit |

If the message was deleted in Slack, or can no longer be edited, the next change is posted as a new message.

If pressing a button shows an error in Slack, check that the Request URL is reachable and exact, and look in Beacon's log for `slack request refused`, which means the signing secret does not match the app.
