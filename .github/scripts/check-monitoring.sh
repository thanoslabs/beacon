#!/usr/bin/env bash
# Checks what Beacon ships for monitoring itself: the alerting rules in
# dev/prometheus and the Grafana dashboard in dev/grafana/dashboards.
#
#   check-monitoring.sh rules   promtool checks the rules and runs their tests
#   check-monitoring.sh live    against the running Compose profile "grafana":
#                               Prometheus scrapes Beacon and has loaded the
#                               rules, Grafana has the dashboard, and every
#                               query of the dashboard is one Prometheus accepts
#
# promtool is the one in the Prometheus image that compose.yaml pins, so the
# rules are checked by the version they are shown to work with.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
prometheus="http://127.0.0.1:${BEACON_PROMETHEUS_PORT:-9090}"
grafana="http://127.0.0.1:${BEACON_GRAFANA_PORT:-3000}"

promtool() {
  docker compose --profile grafana run --rm --no-deps --entrypoint promtool prometheus "$@"
}

case "${1:-}" in
  rules)
    promtool check config /etc/prometheus/prometheus.yml
    promtool check rules /etc/prometheus/beacon-alerts.yml
    promtool test rules /etc/prometheus/beacon-alerts.test.yml
    ;;
  live)
    health="" healthy=""
    # The target is scraped: wait for the first scrape rather than a time.
    for _ in $(seq 1 60); do
      health="$(curl --fail --silent "$prometheus/api/v1/targets" | jq -r '.data.activeTargets[] | select(.labels.job == "beacon") | .health' || true)"
      if [ "$health" = "up" ]; then break; fi
      sleep 1
    done
    if [ "$health" != "up" ]; then
      echo "::error::Prometheus does not scrape Beacon: the target is '${health}'"
      exit 1
    fi
    # Every rule of the file is loaded and evaluates.
    want="$(grep -c '^      - alert:' dev/prometheus/beacon-alerts.yml)"
    for _ in $(seq 1 60); do
      healthy="$(curl --fail --silent "$prometheus/api/v1/rules" | jq '[.data.groups[].rules[] | select(.health == "ok")] | length')"
      if [ "$healthy" = "$want" ]; then break; fi
      sleep 1
    done
    if [ "$healthy" != "$want" ]; then
      echo "::error::${healthy} of ${want} alerting rules are loaded and healthy"
      curl --silent "$prometheus/api/v1/rules" | jq '.data.groups[].rules[] | select(.health != "ok") | {name, health, lastError}'
      exit 1
    fi
    # Grafana has provisioned the dashboard as it is in the repository.
    panels="$(curl --fail --silent --user admin:admin "$grafana/api/dashboards/uid/beacon" | jq '.dashboard.panels | length')"
    if [ "$panels" != "$(jq '.panels | length' dev/grafana/dashboards/beacon.json)" ]; then
      echo "::error::Grafana has ${panels} panels of the Beacon dashboard"
      exit 1
    fi
    # Every query of the dashboard is valid PromQL over metrics that exist.
    # The stat panels of the first row must also have a value: they are
    # written to show a zero, not nothing.
    failed=0
    queries=0
    while IFS=$'\t' read -r type title expr; do
      queries=$((queries + 1))
      answer="$(curl --silent --get "$prometheus/api/v1/query" --data-urlencode "query=${expr//\$__rate_interval/5m}")"
      if [ "$(jq -r '.status' <<< "$answer")" != "success" ]; then
        echo "::error::the panel '${title}' has a query Prometheus refuses: $(jq -r '.error' <<< "$answer")"
        failed=1
      elif [ "$type" = "stat" ] && [ "$(jq '.data.result | length' <<< "$answer")" = "0" ]; then
        echo "::error::the panel '${title}' shows nothing: ${expr}"
        failed=1
      fi
    done < <(jq -r '.panels[] | select(.targets) | . as $p | .targets[] | [$p.type, $p.title, .expr] | @tsv' dev/grafana/dashboards/beacon.json)
    if [ "$failed" -ne 0 ] || [ "$queries" -lt 20 ]; then
      echo "${queries} queries were tried"
      exit 1
    fi
    echo "Prometheus scrapes Beacon, ${want} rules are healthy, and all ${queries} queries of the dashboard are answered."
    ;;
  *)
    echo "usage: $0 rules|live" >&2
    exit 2
    ;;
esac
