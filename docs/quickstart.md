---
title: Quickstart
tags: [guide, quickstart]
date: 2026-11-23
---

From nothing to a page in about five minutes: Beacon and Postgres on your machine, a test alert, the alert in the web interface, and the same alert as a message in Slack or Discord.

You need [Docker](https://docs.docker.com/get-docker/) with Compose, and `git`. Everything below is typed in a terminal, in the directory of the checkout.

## 1. Start Beacon

```sh
git clone https://github.com/thanoslabs/beacon.git
cd beacon
docker compose up --build -d --wait
```

That builds Beacon from the checkout and starts it with Postgres, its only dependency. The first build takes a few minutes; after that a start takes seconds. When the command returns, Beacon is ready:

```sh
curl http://localhost:8080/readyz
```

```json
{"status":"ok","checks":{"database":"ok"}}
```

Beacon runs the configuration in [`examples/quickstart`](https://github.com/thanoslabs/beacon/tree/main/examples/quickstart): one source that alerts are sent to, and a sign-in for the web interface.

If port 8080 is taken, choose another with `BEACON_PORT=18080 docker compose up --build -d --wait`, and use it in the addresses below.

## 2. Sign in

Open <http://localhost:8080> and sign in with the password of the built-in admin:

```
local-development-password
```

There are no alerts yet.

> [!warning] For your own machine only
> That password, and the token that alerts are sent with, are written in `compose.yaml` so that a first run needs no setup. Everyone can read them there. Compose publishes Beacon on `localhost` only, and Beacon says in its log that it runs on published credentials. Before anything depends on an install, give it its own: [[#Make it yours]].

## 3. Send a test alert

```sh
docker compose exec beacon beacon test-alert
```

```
The test alert is firing: http://127.0.0.1:8080/alerts/0f3c...
```

The alert is in the web interface now, under **Alerts**: *BeaconTestAlert*, critical, firing. Open it to see its timeline. **Acknowledge** it, and the timeline says who did; the command below ends it.

```sh
docker compose exec beacon beacon test-alert --resolve
```

`beacon test-alert` sends the alert the way a monitoring system does, through the source, so what happens to it is what happens to a real alert. Nobody was paged, because this configuration has nobody to page. That is next.

## 4. Get paged in chat

Choose Slack or Discord. Either way you make a copy of the quickstart configuration, turn on one more file in it, and start Beacon with the copy.

### Slack

1. Create the Slack app and invite it to a channel: steps 1 to 6 of [[slack#Set up the Slack app]]. Creating the app from the manifest there takes a minute or two. You come back with a bot token (`xoxb-...`), a signing secret and the ID of the channel.
2. Copy the configuration and turn the Slack file on:

   ```sh
   cp -r examples/quickstart my-beacon
   mv my-beacon/slack.yaml.example my-beacon/slack.yaml
   ```

   In `my-beacon/slack.yaml`, put the ID of your channel in place of `C0123456789`.
3. Start Beacon with the copy and the two credentials:

   ```sh
   export BEACON_CONFIG_DIR=./my-beacon
   export BEACON_SLACK_BOT_TOKEN='xoxb-...'
   export BEACON_SLACK_SIGNING_SECRET='...'
   docker compose up -d --wait
   ```

4. Send the test alert through the policy that pages the channel:

   ```sh
   docker compose exec beacon beacon test-alert --policy default
   ```

Within a few seconds the channel has a message: **Firing**, critical, *Test alert from beacon test-alert*, with buttons.

### Discord

1. Create the Discord application and add its bot to your server: steps 1 to 6 of [[discord#Set up the Discord application]]. You come back with a bot token, the application ID, the public key and the ID of the channel.
2. Copy the configuration and turn the Discord file on:

   ```sh
   cp -r examples/quickstart my-beacon
   mv my-beacon/discord.yaml.example my-beacon/discord.yaml
   ```

   In `my-beacon/discord.yaml`, put your application ID, public key and channel ID in place of the ones there.
3. Start Beacon with the copy and the bot token, and send the test alert:

   ```sh
   export BEACON_CONFIG_DIR=./my-beacon
   export BEACON_DISCORD_BOT_TOKEN='...'
   docker compose up -d --wait
   docker compose exec beacon beacon test-alert --policy default
   ```

### If no message arrives

```sh
docker compose logs beacon | grep -E '"level":"(WARN|ERROR)"'
```

The log names what is wrong: a channel the app was not invited to, a token that is not the app's. So does the alert's timeline in the web interface, where every delivery attempt is an entry with its error. `docker compose exec beacon beacon validate --config /etc/beacon --check-secrets` checks the files that Beacon was given ([[configuration#Check the configuration]]).

## What you have, and what comes next

You have an alert that comes in through a source, is grouped and routed, pages a channel through an escalation policy, and can be watched and acknowledged in the web interface.

Three things are not part of these five minutes, because each needs something of yours:

- **The buttons in chat need Beacon to be reachable from the chat service.** Slack and Discord send a button press to Beacon's address, and `localhost` is not one they can reach. Run Beacon where it has a public HTTPS address, or put a tunnel in front of it, and give that address to the app ([[slack#Request URL]], [[discord#Interactions endpoint]]).
- **Acknowledging from chat needs people, and people need an identity provider.** Anyone who sees the message may resolve the alert. Acknowledging and snoozing say "I have this", so Beacon wants to know who is saying it: a person who has signed in to Beacon through your identity provider, and whose chat account is linked to them ([[access]], [[slack#Who can act]]). The built-in admin you signed in with is not a person and has no chat account.
- **Real alerts need a real source.** Point your Alertmanager or Grafana at Beacon ([[alertmanager]], [[grafana]]), or send JSON from anything that can ([[webhook]]).

From here:

- [[configuration]]: sources, routing rules and escalation policies, and how to check the files in CI.
- [[schedules]] and [[escalation]]: a rotation, and a policy that pages whoever is on call and then the next person. [`examples/on-call`](https://github.com/thanoslabs/beacon/tree/main/examples/on-call) has both.
- [[metrics]]: Beacon's own metrics, alerting rules for Beacon itself, and a Grafana dashboard.

## Make it yours

Before anything depends on the install, replace the two published credentials. Compose reads them from your environment:

```sh
export BEACON_SOURCE_TOKEN="$(openssl rand -hex 32)"
docker compose exec beacon beacon hash-password
```

`hash-password` asks for a password of at least 12 characters and prints its hash. Put the hash in a file that only you can read and name the file in the configuration, with `password_hash: {file: /path}` in place of `{env: ...}`; that avoids quoting the `$` signs of a hash in a shell. Better still, set up sign-in through your identity provider and take `bootstrap_admin` out ([[access]]).

For anything beyond your own machine, run the released image with your own Postgres instead of this Compose file:

```sh
docker run --rm -p 8080:8080 \
  -e BEACON_DATABASE_URL='postgres://user:password@host:5432/beacon' \
  -e BEACON_SOURCE_TOKEN -e BEACON_CONFIG=/etc/beacon \
  -v "$PWD/my-beacon:/etc/beacon:ro" \
  ghcr.io/thanoslabs/beacon:X.Y.Z
```

## Stop

```sh
docker compose down
```

The alerts stay in the Postgres volume. `docker compose down --volumes` removes them too.

## What is not in this release

Beacon does alerting and paging. These are planned and not built; the [[requirements]] have the full list.

- **SMS, voice calls and a mobile app.** Pages go to Slack, Discord, email and outbound webhooks.
- **Teams and Telegram.**
- **Heartbeat checks**, a direct Prometheus endpoint, email ingestion and the Datadog, CloudWatch and Sentry integrations. Alerts come from Alertmanager, Grafana alerting and the JSON webhook.
- **Inhibition, maintenance windows and flap detection.** There are silences.
- **Layered and follow-the-sun schedules, shift swaps and calendar feeds.** There are rotations and overrides.
- **Round-robin and conditional escalation steps**, and paging again for a group that stays unresolved.
- **Local users with passwords, SCIM, fine-grained roles, an audit log and multi-tenancy.** People sign in through an OpenID Connect provider and are admins or members.
- **Editing the configuration in the web interface**, a Terraform provider and Kubernetes resources. Sources, routing, policies, teams and schedules are YAML files.
- **A Helm chart**, and built-in reports. There is a container image, a Compose file, and metrics with a dashboard.
- **Incidents, postmortems and status pages.** Beacon is not an incident management tool.
