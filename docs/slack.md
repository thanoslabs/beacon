---
title: Slack
tags: [notifications, slack, guide]
date: 2026-11-16
---

Beacon posts each alert to a Slack channel as one message, and edits that message in place when the alert is acknowledged, snoozed or resolved, so the channel always shows the current state. Delivery, retries and tracking are described in [[notifications]].

> [!warning] Not yet verified against Slack itself
> This integration was built from Slack's API documentation and tested against a fake Slack that checks requests against the documented shapes and limits. Nobody has run it against a real workspace yet. If something below does not match what Slack shows you, please open an issue.

## Set up the Slack app

You need permission to add apps to your workspace.

1. Open <https://api.slack.com/apps?new_app=1> and choose to create the app **from a manifest**. Pick your workspace, and paste this manifest:

   ```yaml
   display_information:
     name: Beacon
   features:
     bot_user:
       display_name: Beacon
       always_online: true
   oauth_config:
     scopes:
       bot:
         - chat:write
         - chat:write.public
   ```

2. **Install the app** to your workspace from the app's settings and approve the permissions it asks for.
3. Copy the **Bot User OAuth Token** from the app's *OAuth & Permissions* page. It starts with `xoxb-`. This is `BEACON_SLACK_BOT_TOKEN`.
4. Choose the channel for alerts and find its ID: open the channel's details in Slack, and the ID (like `C0123456789`) is at the bottom of the *About* tab. This is `BEACON_SLACK_CHANNEL`.
5. If the channel is private, invite the app to it: type `/invite @Beacon` in the channel.

Slack renames the pages and buttons of its settings from time to time, so the names in steps 2 to 5 may differ from what you see.

### Scopes

| Scope | Why |
|---|---|
| `chat:write` | Post messages, and edit the messages Beacon posted |
| `chat:write.public` | Post to public channels without being invited first. Leave it out if you prefer to invite the app to each channel. |

Beacon asks for nothing else: it does not read messages, channels or users.

## Configure Beacon

> [!warning] Temporary settings
> Integrations will be defined in configuration files (see [[configuration-and-data-model]]). Until those exist, `beacon serve` takes one Slack integration and one channel from the settings below, and every alert is posted to that channel.

| Setting | Environment variable | Meaning |
|---|---|---|
| `--slack-bot-token` | `BEACON_SLACK_BOT_TOKEN` | The bot token. Prefer the environment variable: a flag is visible in the process list. |
| `--slack-channel` | `BEACON_SLACK_CHANNEL` | The channel to post to. Use the channel ID. The name of a public channel also works, with or without `#`, but breaks when the channel is renamed. |
| `--slack-api-url` | `BEACON_SLACK_API_URL` | Base URL of the Slack Web API. Leave it alone; it exists to test against a fake Slack. |

The token and the channel must be given together. Without them Beacon stores alerts and notifies nobody.

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
export BEACON_SLACK_BOT_TOKEN='xoxb-...'
export BEACON_SLACK_CHANNEL='C0123456789'
beacon serve
```

Then send an alert as described in [[webhook]], and a message appears in the channel.

## The message

A message shows:

- the state (firing, acknowledged, snoozed or resolved) and the severity;
- the `summary` annotation, or the `alertname` label when there is none;
- the `description` annotation, when present;
- a **Runbook** link, when the `runbook_url` annotation holds an `http` or `https` address;
- the labels;
- what last happened: since when it fires, or who acknowledged, snoozed or resolved it and when, shown in each reader's time zone.

Text from an alert is escaped, so an alert cannot add links or mentions such as `@channel` to the message.

## When something goes wrong

Every attempt is recorded on the alert's timeline with Slack's error. The common ones:

| Error | Meaning | What Beacon does |
|---|---|---|
| `not_in_channel` | The app is not in the channel | Gives up at once. Invite the app, or add `chat:write.public`. |
| `channel_not_found` | The channel does not exist, or is private and the app is not in it | Gives up at once |
| `invalid_auth`, `token_revoked`, `account_inactive` | The bot token is wrong or no longer valid | Gives up at once |
| `missing_scope` | The app lacks `chat:write` | Gives up at once |
| Rate limited (HTTP 429) | Too many messages | Retries, no sooner than Slack asks |
| HTTP 5xx, timeouts, `internal_error` and codes Beacon does not know | Slack is having trouble | Retries with backoff, up to the limit |

If the message was deleted in Slack, or can no longer be edited, the next change is posted as a new message.
