---
title: Discord
tags: [notifications, discord, guide]
date: 2026-11-19
---

Beacon posts each alert group to a Discord channel as one message, and pages people in a direct message that looks the same. It edits a message in place when the alert changes, so every destination shows the current state. Delivery, retries and tracking are described in [[notifications]].

> [!warning] Not yet verified against Discord itself
> This integration was built from Discord's developer documentation (API version 10, as it stood in November 2026) and tested against a fake Discord that checks requests against the documented shapes and limits. Nobody has run it against Discord yet. If something below does not match what Discord shows you, please open an issue.

## Set up the Discord application

You need permission to add apps to your server (*Manage Server*).

1. Open <https://discord.com/developers/applications> and create a **New Application**. Call it Beacon.
2. On the application's *General Information* page, copy the **Application ID** and the **Public Key**. These two are not secrets; they go into the configuration file.
3. On the *Bot* page, press **Reset Token** and copy the token. This is `BEACON_DISCORD_BOT_TOKEN`. Discord shows it once.
4. Add the bot to your server. Open this address in a browser, with your application ID in place of `APPLICATION_ID`, pick the server and approve:

   ```
   https://discord.com/oauth2/authorize?client_id=APPLICATION_ID&scope=bot&permissions=19456
   ```

5. Choose the channel for alerts and find its ID: turn on *Developer Mode* under *User Settings → Advanced*, then right-click the channel and choose **Copy Channel ID**. This is the `channel` in the configuration below.
6. If the channel is private, give the Beacon bot access to it in the channel's permissions.

Discord renames the pages and buttons of its settings from time to time, so the names in these steps may differ from what you see.

### Permissions

The address in step 4 asks for three permissions, and Beacon needs no others:

| Permission | Why |
|---|---|
| *View Channel* | See the channel it posts to |
| *Send Messages* | Post the message of an alert group |
| *Embed Links* | Show the framed part of the message, which holds the description, the labels and the links |

Beacon uses Discord's HTTP API only. It does not connect to the gateway, so it needs no privileged intents, reads no messages, and its bot is shown as offline. That is expected.

## Configure Beacon

A Discord integration is defined in the configuration files (see [[configuration]]). The bot token is never written there: the file names the environment variable, or the file, that holds it.

```yaml
integrations:
  - name: discord
    type: discord
    discord:
      bot_token: {env: BEACON_DISCORD_BOT_TOKEN}
      application_id: "1234567890123456789"
      public_key: 5f1c0c8e7a9b4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e

escalation_policies:
  - name: default
    steps:
      - notify:
          - channel: "1098765432109876543"
            integration: discord
        timeout: 5m

routing:
  default:
    escalation_policy: default
```

| Field | Meaning |
|---|---|
| `bot_token` | The bot token from step 3 |
| `application_id` | The application ID from step 2, in quotes |
| `public_key` | The public key from step 2 |
| `channel` | The channel ID from step 5, in quotes. Discord does not take a channel by name, and `beacon validate` refuses one. |

> [!note] Which alerts go where
> An alert group is posted to the channels named in the **first step** of its escalation policy. Routing rules send alerts to other policies, and so to other channels: see [[configuration#How an alert is routed]]. A policy can also page people and go on to further steps: see [[escalation]].

[`examples/discord`](https://github.com/thanoslabs/beacon/tree/main/examples/discord) holds this configuration with a webhook source. Copy it, put in your IDs and your public key, and start Beacon:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
export BEACON_DISCORD_BOT_TOKEN='...'
beacon serve --config ./my-config
```

`--discord-api-url` (`BEACON_DISCORD_API_URL`) is a process setting, not part of the integration. Leave it alone; it exists to test against a fake Discord.

Then send an alert as described in [[webhook]], and a message appears in the channel.

## The message

A message is about one alert group and is edited in place as the group changes. Its first lines say the state (firing, acknowledged, snoozed or resolved), the severity, and what the alert is: the `summary` annotation, or the `alertname` label when there is none. That is also what a notification on a phone shows. Under it, in a frame whose colour follows the state, a group of one alert shows:

- the `description` annotation, when present;
- a **Runbook** link, when the `runbook_url` annotation holds an `http` or `https` address, and the links the source sent with the alert: **Source**, **Dashboard**, **Panel** and **Silence at the source**;
- **Labels**: the labels of the alert;
- **Status**: what last happened, since when it fires, or who acknowledged, snoozed or resolved it and when, shown in each reader's time zone.

For a group of several alerts the frame shows instead how many alerts are in which state, a list of them, those that fire first, each with its `summary` and the labels that tell it apart (ten are listed and the rest are counted), and the labels the alerts share.

Once the group's escalation policy has paged people, a **Paged** line says who ([[escalation]]):

- A person whose Discord account is linked is shown as a mention, so the name is the one your server knows. **The mention notifies nobody**: a person is paged by a direct message, not by being mentioned in a channel.
- A person who was to be paged and has no linked account is named in plain text, followed by **(Discord not connected)**, for example `Grace Hopper (Discord not connected)`. They were not notified. Whoever reads the message can see that and tell them.

Alerts that a silence keeps quiet are left out of the message for as long as the silence covers them; see [[silences]].

**When Beacon asks again, it says the alert again.** An edit to a message notifies nobody in Discord. So when a step of an escalation policy reaches a channel or a person that already has the group's message, because the policy repeats or because a snooze ran out, Beacon posts a new message and keeps that one up to date from then on. The old message is turned into a one-line pointer, "Beacon sent this alert again".

Text from an alert is escaped, and no message is allowed to mention anyone, so an alert cannot format the message, add links, or ping people with `@everyone`.

## People

A person is paged in a **direct message** from the bot, which looks like the message in a channel and is edited in place like it. For that, Beacon must know which Discord account belongs to which of its users.

- Discord does not let an application look a person up by email address, so **each person links their Discord account themselves**; [[access#Chat accounts]] describes that.

> [!note] Linking is not available yet
> The part of Beacon that Discord talks to, which carries the link command and the buttons of a message, arrives with the next change. Until then nobody's Discord account can be linked, so people are not paged through Discord and messages carry no buttons: Discord delivers to channels only.

- The person must **share a server with the bot** and allow direct messages from members of that server (*Privacy Settings* of the server). Otherwise Discord refuses the message with error code `50007`, and the attempt is recorded as failed on the alert's timeline.
- A person whose account is not linked cannot be paged: the step skips them, its event on the timeline says so, and the group's messages name them with **(Discord not connected)**.
- Without `via`, a person is paged through every Slack and Discord integration in the configuration in which they have an account; `via: [discord]` on a target narrows that ([[escalation#How a person is reached]]).

## Rate limits

Discord limits how often a bot may post to a channel, and says so in its answers. Beacon follows them: when Discord reports that a channel is used up, or answers `429`, Beacon holds back what it has for that channel and delivers it when Discord says it may, through the ordinary retries ([[notifications#Retries]]). A burst of alerts is therefore delivered a little later rather than lost.

## When something goes wrong

Every attempt is recorded on the alert's timeline with Discord's HTTP status and error code. The common ones:

| Error | Meaning | What Beacon does |
|---|---|---|
| `HTTP status 401` | The bot token is wrong or was reset | Gives up at once |
| `error code 50001` | The bot cannot see the channel | Gives up at once. Add the bot to the server and give it *View Channel* there. |
| `error code 50013` | The bot lacks a permission in the channel | Gives up at once. It needs *View Channel*, *Send Messages* and *Embed Links*. |
| `error code 10003` | The channel does not exist, or the ID is not a channel's | Gives up at once |
| `error code 50007` | A person was to be paged and does not accept direct messages from the bot | Gives up at once |
| Rate limited (HTTP 429) | Too many messages | Retries, no sooner than Discord asks |
| HTTP 5xx and timeouts | Discord is having trouble | Retries with backoff, up to the limit |

If the message was deleted in Discord, the next change is posted as a new message.
