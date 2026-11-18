---
title: Slack
tags: [notifications, slack, guide]
date: 2026-11-16
---

Beacon posts each alert to a Slack channel as one message with buttons to acknowledge, snooze and resolve it. It edits that message in place when the alert changes, so the channel always shows the current state. Delivery, retries and tracking are described in [[notifications]].

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
| `chat:write` | Post messages, and edit the messages Beacon posted |
| `chat:write.public` | Post to public channels without being invited first. Leave it out if you prefer to invite the app to each channel. |

Beacon asks for nothing else: it does not read messages, channels or users.

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

> [!warning] Which alerts go where
> Routing and escalation are not implemented yet. Until they are, Beacon posts **every** alert to the channels named in the **first step** of the escalation policy that `routing.default` names, and ignores the rest: later steps, timeouts, people and routing rules. The configuration above keeps its meaning when they arrive.

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

A message shows:

- the state (firing, acknowledged, snoozed or resolved) and the severity;
- the `summary` annotation, or the `alertname` label when there is none;
- the `description` annotation, when present;
- a **Runbook** link, when the `runbook_url` annotation holds an `http` or `https` address;
- the labels;
- the links the source sent with the alert, when they are `http` or `https` addresses: **Source** for the link to where the alert was generated, which Alertmanager sends;
- what last happened: since when it fires, or who acknowledged, snoozed or resolved it and when, shown in each reader's time zone.

Text from an alert is escaped, so an alert cannot add links or mentions such as `@channel` to the message.

## Buttons

A message carries a button for each thing that can be done to the alert in its current state:

| Button | Effect | Offered when the alert is |
|---|---|---|
| **Acknowledge** | Marks the alert acknowledged | firing or snoozed |
| **Snooze 1h** | Snoozes it for one hour, after which it fires again | firing or acknowledged |
| **Resolve** | Resolves it | not yet resolved |

The message then changes to show the new state, who acted and when, and the buttons that still apply. The action is recorded on the alert's timeline with the Slack user who pressed the button (see [[alert-lifecycle]]).

If the alert has changed in the meantime, for example because someone else acknowledged it a moment earlier, nothing happens and Slack shows a note that only you can see, such as "Nothing changed: this alert is already acknowledged."

> [!note] Who can act
> Anyone who can see the message in Slack can press its buttons. Beacon has no users of its own yet; once it does, Slack accounts will be linked to them and the same actions will be attributed to the Beacon user.

### Request URL

When a button is pressed, Slack sends a request to the app's interactivity **Request URL**. Set it to:

```
https://<your Beacon address>/api/v1/integrations/slack/interactions
```

`slack` in the path is the name of the integration. The manifest above sets the URL; to change it later, open *Interactivity & Shortcuts* in the app's settings.

- Slack must be able to reach this address from the internet, over `https`. Only this one path needs to be reachable from outside. To try Beacon from a laptop, a tunnelling tool that gives you a public `https` address works.
- Beacon accepts a request on this path only when it carries a valid signature made with the signing secret and is less than five minutes old. Anything else gets `401` and changes nothing.
- Slack waits three seconds for the answer. If Beacon is down or slow, Slack shows the person an error next to the button.

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

If pressing a button shows an error in Slack, check that the Request URL is reachable and exact, and look in Beacon's log for `slack request refused`, which means the signing secret does not match the app.
