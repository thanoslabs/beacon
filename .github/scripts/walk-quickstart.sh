#!/usr/bin/env bash
# Walks docs/quickstart.md from a fresh checkout, command by command, so
# that the guide cannot rot: every command a reader is told to type is run
# here, in the order of the guide, and each step is checked by what the
# guide says the reader will see. CI runs it in the docker job.
#
# Where the guide needs something of the reader's, a stand-in is used and
# said so: the Slack credentials are made up, so the page itself is not
# delivered; that Beacon queues it for the right channel is checked instead.
#
# Run it from a checkout whose Compose project has no data yet, as a fresh
# clone has. BEACON_PORT moves Beacon off port 8080, as the guide says.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
port="${BEACON_PORT:-8080}"
url="http://localhost:${port}"
started="$(date +%s)"
step() { echo; echo "== $1 ($(( $(date +%s) - started ))s)"; }
psql() { docker compose exec -T postgres psql --username beacon --no-align --tuples-only --command "$1"; }
# count_is <n> <query>: the query answers n.
count_is() { test "$(psql "$2")" = "$1"; }
warned() { docker compose logs beacon | grep -E '"level":"(WARN|ERROR)"' | grep -v 'of the quickstart' | grep -q .; }
# wait_for <what> <command...>: until the command succeeds, for a minute.
wait_for() {
  local what="$1"; shift
  for _ in $(seq 1 60); do
    if "$@" > /dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "::error::timed out waiting for ${what}"
  return 1
}

step "1. Start Beacon"
docker compose up --build -d --wait
# The port is published a moment after the container is healthy.
wait_for "Beacon to answer on ${url}" curl --fail --silent "${url}/readyz"
curl --fail --silent "${url}/readyz" | grep '"status":"ok"'

# The guide starts from nothing, and the checks below count what they find.
if ! count_is 0 "SELECT count(*) FROM alerts"; then
  echo "::error::this database has alerts already: the walk needs a fresh one. In a checkout you only use for this, \`docker compose down --volumes\` removes it."
  exit 1
fi

step "2. Sign in"
# The page a browser opens, and the sign-in the page makes with the password
# of the guide.
curl --fail --silent "${url}/" | grep '<div id="root">' > /dev/null
password="$(grep -A 3 'sign in with the password of the built-in admin' docs/quickstart.md | grep -v '^$' | sed -n '3p')"
cookies="$(mktemp)"
curl --fail --silent --cookie-jar "$cookies" --request POST "${url}/auth/bootstrap" \
  --header 'Content-Type: application/json' --header "Origin: ${url}" --data "{\"password\": \"${password}\"}" > /dev/null
curl --fail --silent --cookie "$cookies" "${url}/api/v1/me" | grep '"role":"admin"' > /dev/null
test "$(curl --fail --silent --cookie "$cookies" "${url}/api/v1/alerts" | jq '.alerts | length')" = "0"
# Beacon says that it runs on published credentials.
docker compose logs beacon | grep -c 'of the quickstart, which is published' | grep -x 2 > /dev/null

step "3. Send a test alert"
docker compose exec -T beacon beacon test-alert | tee /dev/stderr | grep "^The test alert is firing: ${url}/alerts/" > /dev/null
# What the web interface shows under Alerts.
curl --fail --silent --cookie "$cookies" "${url}/api/v1/alerts" | jq -e '.alerts | length == 1 and .[0].labels.alertname == "BeaconTestAlert" and .[0].status == "firing" and .[0].severity == "critical"' > /dev/null
# Acknowledge it, as the guide suggests, the way the page does.
alert="$(curl --fail --silent --cookie "$cookies" "${url}/api/v1/alerts" | jq -r '.alerts[0].id')"
curl --fail --silent --cookie "$cookies" --request POST --header "Origin: ${url}" "${url}/api/v1/alerts/${alert}/acknowledge" | jq -e '.status == "acknowledged"' > /dev/null
curl --fail --silent --cookie "$cookies" "${url}/api/v1/alerts/${alert}/timeline" | jq -e '[.events[].type] | index("acknowledged") != null' > /dev/null
docker compose exec -T beacon beacon test-alert --resolve | tee /dev/stderr | grep '^The test alert is resolved: ' > /dev/null

step "4. Get paged in chat: Slack, with made-up credentials"
rm -rf my-beacon
cp -r examples/quickstart my-beacon
mv my-beacon/slack.yaml.example my-beacon/slack.yaml
# "Put the ID of your channel in place of C0123456789."
sed -i.bak 's/C0123456789/C0QUICKSTART/' my-beacon/slack.yaml && rm my-beacon/slack.yaml.bak
export BEACON_CONFIG_DIR=./my-beacon
export BEACON_SLACK_BOT_TOKEN='xoxb-not-a-real-token'
export BEACON_SLACK_SIGNING_SECRET='not-a-real-signing-secret'
docker compose up -d --wait
wait_for "Beacon to answer on ${url}" curl --fail --silent "${url}/readyz"
docker compose exec -T beacon beacon test-alert --policy default | tee /dev/stderr | grep 'paged through the escalation policy "default"' > /dev/null
# Stand-in for "the channel has a message": the first step of the policy ran
# and a page for the channel of the file is in the queue or was attempted.
# Slack refuses the made-up token, which is the failure the guide's last
# part is about.
wait_for "the page to be queued for the channel" count_is 1 "SELECT count(*) FROM notifications WHERE destination = 'C0QUICKSTART' AND reason = 'escalation_step'"
wait_for "the escalation step on the timeline" count_is 1 "SELECT count(*) FROM alert_events WHERE type = 'escalated' AND data->>'policy' = 'default'"

step "If no message arrives"
# Slack refuses the made-up token (or cannot be reached): either way the
# attempt is an entry on the alert's timeline with its error, and a line in
# the log, which is where the guide sends the reader.
wait_for "the failed delivery on the timeline" count_is 1 "SELECT (count(*) > 0)::int FROM alert_events WHERE type = 'notification_failed' AND data->>'error' <> ''"
wait_for "a warning or an error in the log" warned
docker compose logs beacon | grep -E '"level":"(WARN|ERROR)"' | grep -v 'of the quickstart' | tail -n 2
docker compose exec -T beacon beacon validate --config /etc/beacon --check-secrets | grep 'is valid: 1 source, 1 integration'

step "Make it yours"
# The two commands that make credentials: a token, and a hash that Beacon
# accepts as one.
token="$(openssl rand -hex 32)"
test "${#token}" = "64"
printf 'a-password-of-my-own\n' | docker compose exec -T beacon beacon hash-password | grep '^\$2a\$12\$' > /dev/null

step "Stop"
docker compose down
rm -rf my-beacon "$cookies"
echo
echo "The quickstart was walked in $(( $(date +%s) - started )) seconds."
