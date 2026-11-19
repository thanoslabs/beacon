---
title: Discord
tags: [notifications, discord, guide]
date: 2026-11-19
---

Beacon posts each alert group to a Discord channel as one message with buttons to acknowledge, snooze and resolve it, and pages people in a direct message that looks the same. It edits a message in place when the alert changes, so every destination shows the current state. Delivery, retries and tracking are described in [[notifications]].

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
7. Configure Beacon as described below and start it. Then, back on the application's *General Information* page, set the **Interactions Endpoint URL** to the address below and save. Discord checks the address when you save, so Beacon must be running and reachable; see [Interactions endpoint](#interactions-endpoint).

   ```
   https://<your Beacon address>/api/v1/integrations/discord/interactions
   ```

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
| `public_key` | The public key from step 2, with which Beacon checks that a button press really comes from Discord |
| `channel` | The channel ID from step 5, in quotes. Discord does not take a channel by name, and `beacon validate` refuses one. |

The name of the integration is part of the address that Discord sends button presses to: `/api/v1/integrations/<name>/interactions`.

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

> [!warning] Acknowledging needs sign-in
> **Acknowledging and snoozing from Discord need a Beacon user whose Discord account is linked.** With a configuration that has no `auth` block, nobody can sign in and Beacon has no users: the alert arrives in Discord and can be resolved there, and the *Acknowledge* and *Snooze* buttons answer with a note. To acknowledge from Discord, add sign-in through your identity provider ([[access]]) and have each person link their account. See [Who can act](#who-can-act).

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

## Buttons

A message carries a button for each thing that can be done to the alert in its current state:

| Button | Effect | Offered when the alert is |
|---|---|---|
| **Acknowledge** | Marks the alert acknowledged | firing or snoozed |
| **Snooze 1h** | Snoozes it for one hour, after which it fires again | firing or acknowledged |
| **Resolve** | Resolves it | not yet resolved |

A press acts on the whole group: every alert of the group whose state allows the action takes it, and a button is offered when at least one alert does. The message then changes to show the new state, who acted and when, and the buttons that still apply. The action is recorded on the alert's timeline with the Discord account that pressed the button (see [[alert-lifecycle]]).

If the alert has changed in the meantime, for example because someone else acknowledged it a moment earlier, nothing happens and Discord shows a note that only you can see, such as "Nothing changed: this alert is already acknowledged."

### Who can act

| The person who presses | Acknowledge | Snooze | Resolve |
|---|---|---|---|
| has a Discord account linked to a Beacon user ([People](#people)) | yes | yes | yes |
| is anyone else who can see the message | no | no | yes |

- **Acknowledging and snoozing need a Beacon user.** They say "I have this", and that only means something from a person Beacon knows: the acknowledgement is recorded as theirs, and it stops the escalation.
- **Anyone who can see the message may resolve.** Resolving states a fact about the alert, and the person who fixed the problem may be a colleague who has never signed in to Beacon.
- **A refused press changes nothing** and is answered with a note that only that person sees: *Nothing changed: only Beacon users can acknowledge an alert, and your Discord account is not connected to one. Sign in to Beacon at https://beacon.example.com, ask it for a link code for Discord, and give the code to Beacon here with /beacon-link. You can still resolve the alert from here.*
- **Someone who is not a Beacon user and resolves** is named as your server names them, by their nickname there, or else their display name, or else their handle, with a note: the message reads *Resolved by Gus Guest (Discord not connected)*, and the timeline records that name with their Discord account.
- If Beacon cannot check whether an account is linked, because its database does not answer, it does not guess: acknowledging and snoozing answer *Beacon could not check who you are just now*, and resolving works.

### Interactions endpoint

When a button is pressed, or the link command is used, Discord sends a request to the application's **Interactions Endpoint URL** (step 7):

```
https://<your Beacon address>/api/v1/integrations/discord/interactions
```

`discord` in the path is the name of the integration.

- Discord must be able to reach this address from the internet, over `https`. Only this one path needs to be reachable from outside. To try Beacon from a laptop, a tunnelling tool that gives you a public `https` address works.
- When you save the address, Discord sends it a test request and some requests with a wrong signature. Beacon answers the first and refuses the others, which is what Discord wants to see. If saving fails, Beacon is not reachable at that address, or `public_key` in the configuration is not the application's.
- Beacon accepts a request on this path only when it carries a valid signature made by Discord for your application (Ed25519, checked with `public_key`). Anything else gets `401` and changes nothing.
- Discord waits three seconds for the answer. If Beacon is down or slow, Discord shows the person "This interaction failed".

## People

A person is paged in a **direct message** from the bot, which looks like the message in a channel, has the same buttons and is edited in place like it. For that, and to know who presses a button, Beacon must know which Discord account belongs to which of its users.

Discord does not let an application look a person up by email address, so **each person links their Discord account themselves**, once:

1. Sign in to Beacon ([[access]]) and ask it for a link code: `POST /api/v1/me/identities/discord/link-code`, where `discord` is the name of the integration. The answer holds the code and the command to type:

   ```json
   {"integration": "discord", "provider": "discord", "code": "K7QF-2MXA-9DTR-PB4W", "expires_at": "2026-12-01T10:22:00Z", "command": "/beacon-link code: K7QF-2MXA-9DTR-PB4W"}
   ```

2. In Discord, in a server that has the Beacon bot or in a direct message with it, type `/beacon-link` and give it the code. Beacon answers, to you alone, *Connected: this Discord account now belongs to Ada Lovelace in Beacon.*

- A code works **once**, for **ten minutes**, and only for the integration it was issued for. Asking for a new code makes the earlier one useless. Only a hash of a code is stored, and codes are never logged.
- The code proves both ends: only the signed-in person can get it, and Discord vouches, with its signature, for which account typed it.
- An account belongs to one user. If the account is linked to someone else, nothing changes; that person removes the link first.
- **To remove a link**, the person calls `DELETE /api/v1/me/identities/discord`. `GET /api/v1/me/identities` lists what is linked.
- The bootstrap admin and API tokens are not people and cannot link an account.
- Until the web interface exists, the two routes are called with the session of a signed-in browser.

**Beacon registers the `/beacon-link` command by itself**, as a global command of the application, when it starts and when a Discord integration is added. It leaves the application's other commands alone. If that fails, for example because the bot token is wrong, the log says `register the link command with Discord` and Beacon tries again, after a minute and then at growing intervals up to an hour, and at once after a reload.

About paging:

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

If pressing a button shows "This interaction failed", check that the Interactions Endpoint URL is reachable and exact, and look in Beacon's log for `discord request refused`, which means the request was not signed for the `public_key` in the configuration.
