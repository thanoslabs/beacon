---
title: Email
tags: [notifications, email, guide]
date: 2026-11-19
---

Beacon sends alert groups as email, over SMTP: to a shared address, and to people at the address they have in Beacon. An email cannot be changed once it is sent, so every change to a group is a further message, and the messages about one group form one conversation. Delivery, retries and tracking are described in [[notifications]].

> [!note] Tested with
> The channel is tested against a real SMTP server, [Mailpit](https://mailpit.axllent.org/) 1.33, which `compose.yaml` runs for development, and against a scripted server for TLS in both forms. It has not been tried with a hosted mail provider.

## Configure Beacon

An email integration is defined in the configuration files (see [[configuration]]). The password is never written there: the file names the environment variable, or the file, that holds it.

```yaml
integrations:
  - name: email
    type: email
    email:
      host: smtp.example.com
      port: 587
      tls: starttls
      username: beacon
      password: {env: BEACON_SMTP_PASSWORD}
      from: beacon@example.com

escalation_policies:
  - name: default
    steps:
      - notify:
          - channel: oncall@example.com
            integration: email
        timeout: 5m

routing:
  default:
    escalation_policy: default
```

| Field | Required | Meaning |
|---|---|---|
| `host`, `port` | yes | The SMTP server |
| `tls` | yes | How the connection is secured; see below |
| `username`, `password` | no | The account Beacon signs in with. Both or neither. |
| `from` | yes | The sender: one plain address, without a display name |
| `channel` (in a target) | yes | For an email integration, the address the message is sent to |

| `tls` | The connection | Usual port |
|---|---|---|
| `starttls` | Starts unencrypted and is upgraded with `STARTTLS` before anything else is said | 587 |
| `implicit` | Is TLS from the first byte | 465 |
| `none` | Is not encrypted | 25, and only towards a relay you trust |

- **Beacon never falls back to an unencrypted connection.** With `starttls`, a server that does not offer `STARTTLS` is refused. The server's certificate must be valid for `host` and come from an authority the system trusts; TLS 1.2 is the least Beacon accepts.
- **The password travels only over TLS.** `beacon validate` refuses `tls: none` together with a password, unless the server is on the same machine (`localhost`).
- Beacon signs in with `PLAIN`, or with `LOGIN` when the server offers that alone.

[`examples/email`](https://github.com/thanoslabs/beacon/tree/main/examples/email) holds this configuration with a webhook source.

### The link to the alert

A message links to the alert in Beacon when Beacon knows its own address, which is `auth.public_url` ([[access]]). Without it, the log says at start that emails will carry no such link. The link leads to the alert in Beacon's API, which shows it to someone who is signed in; the web interface will take its place when it exists.

## The message

The subject says the state, the severity and what the alert is: `[FIRING] critical: Disk is full on db1`. The body, as plain text and as HTML, holds:

- the state and the severity;
- the `summary` annotation, or the `alertname` label when there is none, and the `description` annotation;
- the labels;
- what last happened: since when it fires, or who acknowledged, snoozed or resolved it and when, in UTC;
- who the escalation policy has paged so far, and who was to be paged and has no linked account in a chat service ([[escalation#Who is skipped]]);
- links: **Open in Beacon**, **Runbook** when the `runbook_url` annotation holds an `http` or `https` address, and the links the source sent with the alert;
- why the message was sent.

For a group of several alerts the subject counts them, `[FIRING] critical: DiskFull (3 alerts)`, and the body lists them with their states in place of the description.

**Every notification is a message.** The first message of a group is followed by one when someone acknowledges, snoozes or resolves, when alerts join the group or resolve (at most once per `group_interval`, see [[configuration#Grouping]]), and when the group is resolved. Each answers the one before (`In-Reply-To` and `References`), so a mail client that groups by those headers shows one conversation per alert group. The subject changes with the state, and some clients start a new conversation when it does.

Text from an alert cannot add headers, recipients or a second message: the subject is stripped of line breaks and encoded, the body is encoded as quoted-printable, and the HTML form escapes everything.

**There is nothing to press.** An email carries no buttons and no links that act on the alert. Acknowledge from chat, or through the API.

## People

A person is reached at **the email address they have in Beacon**, which comes from the identity provider at sign-in ([[access#Users]]). Nothing has to be linked: everyone who has signed in can be sent email.

- Without `via`, a person is notified through every Slack and Discord integration in which they have an account, and through every email integration. `via: [email]` on a target narrows that ([[escalation#How a person is reached]]).
- A person who has never signed in is not a user, and a step skips them like in every channel.
- The bootstrap admin has no address and is never written to.

## When something goes wrong

Every attempt is recorded on the alert's timeline with the step that failed and the SMTP server's reply.

| Error | Meaning | What Beacon does |
|---|---|---|
| A reply that starts with `5`, such as `550` for a mailbox that does not exist or `535` for a wrong password | The server refuses for good | Gives up at once |
| A reply that starts with `4`, such as `451` or `421` | The server refuses for now | Retries with backoff, up to the limit |
| `connect to the SMTP server`, timeouts, a certificate that cannot be verified | The server cannot be reached or trusted | Retries with backoff, up to the limit |
| `does not offer STARTTLS` | The port does not speak `STARTTLS` | Gives up at once. Use `tls: implicit` for a port that is TLS from the start. |
| `the destination is not an email address` | The `channel` of a target is not one plain address | Gives up at once. `beacon validate` finds this beforehand. |

Delivery is at least once ([[notifications#Delivery]]). A message that is sent a second time, because Beacon stopped between the server's acceptance and its own record of it, carries the same `Message-ID` as the first, so a mail system that removes duplicates can.

## Try it locally

`compose.yaml` has an SMTP server that keeps every message and delivers none:

```sh
docker compose up -d --wait mailpit
```

It listens on `localhost:1025`, takes mail from the user `beacon` with the password in `dev/mailpit/auth`, and shows what it received at <http://localhost:8025>. Point an email integration at it with `host: localhost`, `port: 1025`, `tls: none`.
