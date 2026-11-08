#!/usr/bin/env bash
# End-to-end tests of vmalertsproxy behind VictoriaMetrics -vmalert.proxyURL.
#
# Requests are sent the same way Grafana sends them to a Prometheus datasource.
# Run via test/docker-compose.yml.
# shellcheck disable=SC2016 # $vars in single quotes are jq variables.
set -euo pipefail

VM_URL=${VM_URL:-http://victoriametrics:8428}
PARTIAL_URL=${PARTIAL_URL:-http://vmalertsproxy-partial:8880}
HOST_URL=${HOST_URL:-http://vmalertsproxy-host:8880}
GRAFANA_UA=${GRAFANA_UA:-Grafana/12.2.0}
WAIT_TIMEOUT=${WAIT_TIMEOUT:-90}

failures=0

# get prints the response body of a GET request sent as Grafana. Extra args are passed to curl.
get() {
  curl -fsS -G -A "$GRAFANA_UA" "$@"
}

# status prints the response status code.
status() {
  curl -s -o /dev/null -w '%{http_code}' -A "$GRAFANA_UA" "$1"
}

# expect checks that the jq filter is true for the json.
expect() {
  local name=$1 json=$2 filter=$3
  if jq -e "$filter" >/dev/null 2>&1 <<<"$json"; then
    echo "PASS $name"
  else
    echo "FAIL $name"
    echo "     filter:   $filter"
    echo "     response: ${json:0:2000}"
    failures=$((failures + 1))
  fi
}

expect_status() {
  local name=$1 url=$2 want=$3 got
  got=$(status "$url")
  if [[ $got == "$want" ]]; then
    echo "PASS $name"
  else
    echo "FAIL $name: status $got, want $want"
    failures=$((failures + 1))
  fi
}

echo "Waiting for alerts of both sites..."
deadline=$((SECONDS + WAIT_TIMEOUT))
until get "$VM_URL/api/v1/alerts" 2>/dev/null | jq -e '[.data.alerts[] | select(.state == "firing")] | length == 2' >/dev/null 2>&1; do
  if ((SECONDS >= deadline)); then
    echo "FAIL timeout: both vmalert instances must fire AlwaysFiring within ${WAIT_TIMEOUT}s"
    get "$VM_URL/api/v1/rules" || true
    exit 1
  fi
  sleep 1
done

rules=$(get "$VM_URL/api/v1/rules")
expect "identical rules of different sites are kept in separate namespaces" "$rules" '
  [.data.groups[].file] == ["msk: /etc/vmalert/rules.yml", "spb: /etc/vmalert/rules.yml"]'
expect "identical alerting rules are not merged" "$rules" '
  [.data.groups[].rules[] | select(.name == "AlwaysFiring")] | length == 2'
expect "rules are labeled with their site" "$rules" '
  [.data.groups[] | (.file | split(":")[0]) as $site | .rules[] | .labels.vmalert_site == $site] | all'
expect "alert labels are not changed, so Grafana silences match real alerts" "$rules" '
  [.data.groups[].rules[].alerts[]? | .labels | has("vmalert_site") | not] | all'
expect "ids are unique across sites" "$rules" '
  [.data.groups[] | .id, .rules[].id] as $ids | ($ids | length) == ($ids | unique | length)'
expect "vmalert ids are kept after the site prefix" "$rules" '
  [.data.groups[].id | split(":")[1]] | unique | length == 1'
expect "rules have no warnings when all backends are available" "$rules" '.warnings == null'

# Grafana rule view page looks up a rule by namespace, group and name.
rule=$(get "$VM_URL/api/v1/rules" \
  --data-urlencode "file=spb: /etc/vmalert/rules.yml" --data-urlencode "file[]=spb: /etc/vmalert/rules.yml" \
  --data-urlencode "rule_group=demo" --data-urlencode "rule_group[]=demo" \
  --data-urlencode "rule_name=AlwaysFiring" --data-urlencode "rule_name[]=AlwaysFiring")
expect "Grafana rule view finds the rule of the requested site" "$rule" '
  [.data.groups[].rules[]] | length == 1 and .[0].labels.vmalert_site == "spb"'

# Grafana 12+ paginates rules with group_limit and group_next_token.
page1=$(get "$VM_URL/api/v1/rules" -d group_limit=1 -d exclude_alerts=true)
expect "Grafana pagination: first page" "$page1" '
  [.data.groups[].file] == ["msk: /etc/vmalert/rules.yml"] and (.data.groupNextToken | length > 0)'
token=$(jq -r '.data.groupNextToken' <<<"$page1")
page2=$(get "$VM_URL/api/v1/rules" -d group_limit=1 -d exclude_alerts=true --data-urlencode "group_next_token=$token")
expect "Grafana pagination: last page" "$page2" '
  [.data.groups[].file] == ["spb: /etc/vmalert/rules.yml"] and .data.groupNextToken == null'
expect "exclude_alerts removes alerts" "$page1" '[.data.groups[].rules[] | has("alerts") | not] | all'

# vmui paginates rules with group_limit and page_num.
vmui=$(get "$VM_URL/vmalert/api/v1/rules" -d group_limit=1 -d page_num=2)
expect "vmui pagination" "$vmui" '
  .page == 2 and .total_pages == 2 and .total_groups == 2 and [.data.groups[].file] == ["spb: /etc/vmalert/rules.yml"]'

alerts=$(get "$VM_URL/api/v1/alerts")
expect "alerts of both sites are returned" "$alerts" '
  [.data.alerts[].id | split(":")[0]] == ["msk", "spb"]'

filtered=$(get "$VM_URL/api/v1/rules" -d type=record)
expect "rule type filter" "$filtered" '[.data.groups[].rules[].type] == ["recording", "recording"]'

# Grafana filters rules by state and labels, including the site label added by the proxy.
filtered=$(get "$VM_URL/api/v1/rules" -d state=firing)
expect "rule state filter" "$filtered" '[.data.groups[].rules[].name] == ["AlwaysFiring", "AlwaysFiring"]'
filtered=$(get "$VM_URL/api/v1/rules" -d state=ok -d extended_states=true)
expect "extended states" "$filtered" '[.data.groups[].rules[] | "\(.name)=\(.state)"] == ["demo:one=ok", "demo:one=ok"]'
filtered=$(get "$VM_URL/api/v1/rules" --data-urlencode 'match[]={vmalert_site="spb"}')
expect "match[] by the site label" "$filtered" '
  [.data.groups[].file] == ["spb: /etc/vmalert/rules.yml"] and (.data.groups[0].rules | length == 2)'
filtered=$(get "$VM_URL/api/v1/rules" -d search=msk)
expect "search by site" "$filtered" '[.data.groups[].file] == ["msk: /etc/vmalert/rules.yml"]'
filtered=$(get "$VM_URL/api/v1/alerts" --data-urlencode 'file[]=msk: /etc/vmalert/rules.yml')
expect "alerts filtered by namespace" "$filtered" '[.data.alerts[].id | split(":")[0]] == ["msk"]'

invalid=$(curl -s -A "$GRAFANA_UA" "$VM_URL/api/v1/rules?type=bad")
expect "invalid params return a Prometheus API error" "$invalid" '.status == "error" and .errorType == "bad_data"'

# vmui drills down to a rule by group_id and rule_id.
group_id=$(jq -r '.data.groups[1].id' <<<"$rules")
rule_id=$(jq -r '.data.groups[1].rules[] | select(.name == "AlwaysFiring") | .id' <<<"$rules")
single=$(get "$VM_URL/vmalert/api/v1/rule" --data-urlencode "group_id=$group_id" --data-urlencode "rule_id=$rule_id")
expect "single rule is fetched from the site owning it" "$single" "
  .id == \"$rule_id\" and .group_id == \"$group_id\" and .file == \"spb: /etc/vmalert/rules.yml\" and (.updates | length > 0)"
group=$(get "$VM_URL/vmalert/api/v1/group" --data-urlencode "group_id=$group_id")
expect "single group is fetched from the site owning it" "$group" ".id == \"$group_id\" and (.rules | length == 2)"

notifiers=$(get "$VM_URL/api/v1/notifiers")
expect "notifiers are merged" "$notifiers" '.status == "success"'

expect_status "unknown site in ids" "$VM_URL/vmalert/api/v1/rule?group_id=nsk:1&rule_id=nsk:2" 404
expect_status "vmalert web UI is not supported" "$VM_URL/vmalert/groups" 501

expect "each site is answered by its own vmalert" "$rules" '
  [.data.groups[] | (.file | split(":")[0]) as $site | .rules[].alerts[]? | .source | startswith("http://vmalert-\($site).example")]
  | length == 2 and all'

# Backends behind vmauth acting as an ingress: HTTPS to a host the certificate isn't valid for,
# with the Host header selecting the vmalert. Host for msk is set in the config file, for spb by a flag.
host=$(get "$HOST_URL/api/v1/rules")
expect "backends behind an ingress are reached by the Host header over HTTPS" "$host" '
  .warnings == null and [.data.groups[].file] == ["msk: /etc/vmalert/rules.yml", "spb: /etc/vmalert/rules.yml"]'
expect "the Host header routes each site to its own vmalert" "$host" '
  [.data.groups[] | (.file | split(":")[0]) as $site | .rules[].alerts[]? | .source | startswith("http://vmalert-\($site).example")]
  | length == 2 and all'
host_rule=$(get "$HOST_URL/api/v1/rule" --data-urlencode "group_id=$group_id" --data-urlencode "rule_id=$rule_id")
expect "single rule requests use the Host header too" "$host_rule" ".id == \"$rule_id\""

partial=$(get "$PARTIAL_URL/api/v1/rules")
expect "partial response contains available sites" "$partial" '
  [.data.groups[].file] == ["msk: /etc/vmalert/rules.yml", "spb: /etc/vmalert/rules.yml"]'
expect "partial response reports the unavailable site" "$partial" '
  .warnings | length == 1 and (.[0] | contains("\"dead\""))'

metrics=$(curl -fsS "$PARTIAL_URL/metrics")
if grep -q '^vmalertsproxy_backend_up{backend="dead"} 0$' <<<"$metrics" &&
  grep -q '^vmalertsproxy_backend_up{backend="msk"} 1$' <<<"$metrics"; then
  echo "PASS backend availability metrics"
else
  echo "FAIL backend availability metrics"
  failures=$((failures + 1))
fi

if ((failures > 0)); then
  echo "$failures check(s) failed"
  exit 1
fi
echo "All checks passed"
