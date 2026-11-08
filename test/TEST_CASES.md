# vmalertsproxy test cases

Every test case points to the automated tests that cover it:

- **Unit**: a Go test, by function name. Run with `docker compose -f test/docker-compose.yml run --rm unit`.
- **E2E**: a check from [e2e.sh](e2e.sh), quoted as it appears in the `PASS` line. Run with
  `docker compose -f test/docker-compose.yml run --rm --build e2e`. The stand has VictoriaMetrics with
  `-vmalert.proxyURL`, two vmalert instances (msk, spb) with the same rules, vmauth acting as an ingress, and a proxy
  with an unavailable backend.
- **Manual**: a check in the Grafana UI on the stand started with
  `docker compose -f test/docker-compose.yml --profile grafana up -d --build`, at http://localhost:3000/alerting/list.

There are currently 75 unit tests and 32 e2e checks.

## MRG. Merging responses and keeping sites apart

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| MRG-01 | Two vmalerts have the same rule files (file, group, expression, labels and IDs all match) | Two groups in different `<site>: <file>` namespaces, identical rules aren't merged | Unit: `TestRulesFromIdenticalBackendsStayDistinct`. E2E: "identical rules of different sites are kept in separate namespaces", "identical alerting rules are not merged" |
| MRG-02 | Group, rule and alert IDs from different sites | IDs look like `<site>:<id>`, are unique after the merge, and keep the original ID after the prefix | Unit: `TestRulesFromIdenticalBackendsStayDistinct`, `TestRewriteDefaults`, `TestComposeAndParseID`. E2E: "ids are unique across sites", "vmalert ids are kept after the site prefix" |
| MRG-03 | Site label on rules | `vmalert_site=<site>` is added to rule labels, existing labels aren't overwritten | Unit: `TestRewriteDefaults`, `TestRewriteCustomTemplatesAndLabels`. E2E: "rules are labeled with their site" |
| MRG-04 | Alert labels by default | Alert labels are unchanged, so a silence created in Grafana matches the alert in Alertmanager | Unit: `TestRulesFromIdenticalBackendsStayDistinct`, `TestSingleAlert`. E2E: "alert labels are not changed, so Grafana silences match real alerts" |
| MRG-05 | `label_alerts: true` | The site label is added to alerts too | Unit: `TestRewriteCustomTemplatesAndLabels`, `TestAlertLabels` |
| MRG-06 | `site_label: ""` | Rule and alert labels are unchanged | Unit: `TestRewriteWithoutSiteLabel` |
| MRG-07 | `rewrite.file` and `rewrite.group` templates with `{site}`, `{file}`, `{group}` | File and group names are built from the original values by the templates, the `file` and `group_name` fields of rules match the group | Unit: `TestRewriteCustomTemplatesAndLabels`, `TestRewriteStandaloneRule` |
| MRG-08 | The vmalert response has fields the proxy doesn't know | The fields pass through unchanged | Unit: `TestGroupRoundTripKeepsUnknownFields`, `TestRulesFromIdenticalBackendsStayDistinct`, `TestSingleAlert` |
| MRG-09 | A backend with the Prometheus API, without vmalert fields (`id`, `states`, `type`) | The proxy doesn't add missing fields, `file` and labels are rewritten, a group without `type` is treated as a Prometheus group | Unit: `TestPrometheusCompatibleBackend`, `TestMarshalDoesNotAddMissingFields`, `TestGroupWithoutRulesMarshalsEmptyList` |
| MRG-10 | Group order | Groups are sorted by `file`, then by name, so groups of one namespace go together | Unit: `TestRulesFromIdenticalBackendsStayDistinct`, `TestRulesTokenPagination` |
| MRG-11 | Repeated requests with different filters over cached data | Filtering and rewriting don't modify the objects shared with the cache | Unit: `TestFilterDoesNotModifySharedGroups`, `TestWithLabelDoesNotModifyInput` |
| MRG-12 | Site data comes from the site's own vmalert | Alerts of a site have a `source` link to its vmalert | E2E: "each site is answered by its own vmalert" |

## GRF. Grafana compatibility

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| GRF-01 | Rules request with `User-Agent: Grafana*` | `datasource_type=prometheus` is set, vlogs and graphite groups aren't returned | Unit: `TestRulesGrafanaGetsPrometheusGroupsOnly` |
| GRF-02 | Grafana passes `datasource_type` explicitly | The value isn't overridden | Unit: `TestGrafanaUserAgentKeepsExplicitDatasourceType` |
| GRF-03 | Rule details page: Grafana looks a rule up by `file`/`file[]`, `rule_group`/`rule_group[]`, `rule_name`/`rule_name[]` | One rule of the requested site is returned | Unit: `TestRulesGrafanaRuleView`. E2E: "Grafana rule view finds the rule of the requested site" |
| GRF-04 | Grafana 12+ pagination: `group_limit` and `group_next_token` | A page has at most `group_limit` groups, `groupNextToken` leads to the next page, the last page has no token, every group is returned exactly once | Unit: `TestRulesTokenPagination`, `TestPaginationOfFilteredGroups`, `TestTokenWithoutLimitReturnsRemainingGroups`, `TestGroupToken`. E2E: "Grafana pagination: first page", "Grafana pagination: last page" |
| GRF-05 | The group a token points to is removed between page requests | The next page starts with the next existing group | Unit: `TestTokenPaginationContinuesAfterRemovedGroup` |
| GRF-06 | Grafana state aliases `noData`, `normal`, `error` | Mapped to `nomatch`, `inactive`, `unhealthy` | Unit: `TestRulesFilters` |
| GRF-07 | Invalid request | An error in the Prometheus API format: `status: error`, `errorType`, `error` | Unit: `TestErrorResponseFormat`. E2E: "invalid params return a Prometheus API error" |
| GRF-08 | Alert rules → Data source-managed | Rules of every site are in their own namespace, identical rules of different sites are shown separately | Manual |
| GRF-09 | `label:vmalert_site=<site>` filter in the rule list | Only the rules of that site are left | Manual |
| GRF-10 | Open a rule details page from the list | The page opens, the Instances tab shows the alerts of that site | Manual |
| GRF-11 | Silence from an alert instance | Matchers have the alert labels without `vmalert_site`, the silence matches the alert in Alertmanager | Manual |

## FLT. `/api/v1/rules` and `/api/v1/alerts` filters

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| FLT-01 | `rule_group[]`, `file[]`, `rule_name[]` and their Mimir forms without `[]` | Matching groups and rules are left, both forms are combined | Unit: `TestRulesFilters`, `TestListParamMergesPrometheusAndMimirForms` |
| FLT-02 | `type=alert` / `type=record` | Rules of the given type are left | Unit: `TestRulesFilters`. E2E: "rule type filter" |
| FLT-03 | `state` and `filter`, several comma-separated values | Rules in the given states are left | Unit: `TestRulesFilters`. E2E: "rule state filter" |
| FLT-04 | `extended_states=true` | The `ok`, `nomatch` and `unhealthy` states are computed the way vmalert does it | Unit: `TestRulesFilters`, `TestExtendState`, `TestBoolParam`. E2E: "extended states" |
| FLT-05 | `match[]`: `=`, `!=`, `=~`, `!~`, `or`, several selectors, the site label | Regexps are anchored, labels must match at least one selector, a missing label equals an empty one | Unit: `TestLabelMatchers`, `TestRulesFilters`. E2E: "match[] by the site label" |
| FLT-06 | `search` | Case-insensitive search by group name and file, otherwise by rule name | Unit: `TestSearch`, `TestRulesFilters`. E2E: "search by site" |
| FLT-07 | `datasource_type` | Groups of the given type are left | Unit: `TestRulesFilters`, `TestGroupsFilterDatasourceType` |
| FLT-08 | `exclude_alerts=true` | The `alerts` field is removed from rules | Unit: `TestRulesExcludeAlerts`, `TestRuleWithoutAlertsOmitsAlerts`, `TestFilterDoesNotModifySharedGroups`. E2E: "exclude_alerts removes alerts" |
| FLT-09 | A rule filter and a group without rules | The group without rules stays in the response, as in vmalert | Unit: `TestRulesFilters`, `TestRulesPostForm` |
| FLT-10 | The `states` field of a group | Recomputed from the filtered rules | Unit: `TestRulesFromIdenticalBackendsStayDistinct`, `TestFilterDoesNotModifySharedGroups` |
| FLT-11 | Invalid `type`, `state`, `match[]`, `group_limit`, `page_num`, `group_next_token` | 400 `bad_data` | Unit: `TestNewRulesFilterErrors`, `TestRulesFilters`, `TestRulesPageNumPagination`, `TestRulesTokenPagination` |
| FLT-12 | Params in the body of a POST request | Applied the same way as in GET | Unit: `TestRulesPostForm` |
| FLT-13 | `/api/v1/alerts` with `rule_group[]`, `file[]`, `datasource_type`, `match[]` | Alerts of alerting rules only, filtered and sorted by ID | Unit: `TestAlerts`, `TestAlertsFilter`. E2E: "alerts of both sites are returned", "alerts filtered by namespace" |

## PGN. vmui pagination

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| PGN-01 | `group_limit` and `page_num` | A page of groups and the `page`, `total_pages`, `total_groups`, `total_rules` counters across all sites, `group_next_token` is ignored | Unit: `TestRulesPageNumPagination`, `TestPageNumIgnoresToken`. E2E: "vmui pagination" |
| PGN-02 | `page_num` past the last page or without `group_limit` | 400 `bad_data` | Unit: `TestRulesPageNumPagination` |

## OBJ. Objects by ID (vmui drill-down)

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| OBJ-01 | `/api/v1/rule?group_id=<site>:<id>&rule_id=<site>:<id>` | The request goes only to the site's vmalert with the original IDs, the response is rewritten, the `updates` history is kept | Unit: `TestSingleRule`. E2E: "single rule is fetched from the site owning it", "single rule requests use the Host header too" |
| OBJ-02 | `/api/v1/group?group_id=<site>:<id>` | The group and its rules are rewritten | Unit: `TestSingleGroup`. E2E: "single group is fetched from the site owning it" |
| OBJ-03 | `/api/v1/alert?group_id=<site>:<id>&alert_id=<site>:<id>` | IDs are rewritten, alert labels are unchanged | Unit: `TestSingleAlert` |
| OBJ-04 | Unknown site, an ID without a prefix, IDs of different sites, no `group_id` | 404, 400, 400, 400 | Unit: `TestSingleRule`. E2E: "unknown site in ids" |
| OBJ-05 | vmalert responds with 404 | The status and body are passed through | Unit: `TestSingleRule`, `TestSingleGroup` |
| OBJ-06 | vmalert is unavailable or responds with broken JSON | 502, the response doesn't contain the backend address | Unit: `TestSingleObjectBackendUnavailable`, `TestSingleObjectMalformedResponse` |

## NTF. Notifiers

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| NTF-01 | `/api/v1/notifiers` | Notifiers of all sites are merged, the site label is added to target labels | Unit: `TestNotifiers`, `TestNotifierRoundTrip`. E2E: "notifiers are merged" |
| NTF-02 | Some or all vmalerts are unavailable | 200 with `warnings`, or 503 | Unit: `TestNotifiersPartialResponse` |

## FLR. Fault tolerance

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| FLR-01 | One site is unavailable | 200 with the data of the other sites, `warnings` names the unavailable one. When all sites are available, there are no `warnings` | Unit: `TestPartialResponse`. E2E: "partial response contains available sites", "partial response reports the unavailable site", "rules have no warnings when all backends are available" |
| FLR-02 | vmalert responds with 5xx, `status: error`, truncated JSON, HTML, JSON of another schema | The site is treated as unavailable, the others are returned | Unit: `TestBackendFailuresArePartial`, `TestUnmarshalErrors` |
| FLR-03 | vmalert doesn't respond | The request is cancelled after `backend_timeout` and doesn't block the response | Unit: `TestBackendFailuresArePartial/timeout` |
| FLR-04 | All sites are unavailable | 503 `unavailable` | Unit: `TestPartialResponse`, `TestNotifiersPartialResponse` |
| FLR-05 | `allow_partial_response: false` and one site is unavailable | 503 | Unit: `TestPartialResponse` |
| FLR-06 | A site recovers while the cache is enabled | The error isn't cached, the next request goes to vmalert | Unit: `TestFailuresAreNotCached` |
| FLR-07 | Backend error with a password in the URL | Responses contain no addresses or secrets, the password is masked in logs | Unit: `TestSecretsAreNotLogged`, `TestSingleObjectBackendUnavailable` |

## CCH. Cache and load on vmalert

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| CCH-01 | Several rules and alerts requests within `cache_ttl` | One request to every vmalert | Unit: `TestRulesCache` |
| CCH-02 | `cache_ttl: 0` | Every request goes to vmalert | Unit: `TestRulesCache` |
| CCH-03 | `cache_ttl` has expired | A new request to vmalert | Unit: `TestCacheExpires` |
| CCH-04 | Concurrent requests | Share one request to vmalert | Unit: `TestConcurrentRequestsShareBackendRequest` |

## CON. Connecting to vmalert

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| CON-01 | Backend URL with a path prefix and query params | The prefix and params are kept | Unit: `TestBackendURLAndAuth` |
| CON-02 | Custom `headers` | Sent with every request | Unit: `TestBackendURLAndAuth` |
| CON-03 | `Host` header in the config or via `-backend.header` | vmalert receives the given `Host` | Unit: `TestBackendURLAndAuth`, `TestConfigFromFlags`. E2E: "backends behind an ingress are reached by the Host header over HTTPS" |
| CON-04 | HTTPS by IP or by another name with `Host`, the certificate is issued for the `Host` name | SNI and certificate verification use `Host`, the ingress routes to the site's vmalert | Unit: `TestBackendHostHeaderTLS`. E2E: "backends behind an ingress are reached by the Host header over HTTPS", "the Host header routes each site to its own vmalert" |
| CON-05 | `insecure_skip_verify: true` and a missing `ca_file` | The proxy starts, the certificate isn't verified | Unit: `TestBackendHostHeaderTLS` |
| CON-06 | Basic auth: `password` and `password_file` | The `Authorization: Basic` header is sent | Unit: `TestBackendURLAndAuth` |
| CON-07 | Bearer token: a value, a file, file rotation, a removed file | The current token is sent, rotation is picked up without a restart, without the file the site is unavailable | Unit: `TestBearerToken` |
| CON-08 | Client certificate (mTLS) | The certificate is sent | Unit: `TestClientCertificate` |
| CON-09 | Missing or invalid `ca_file`, missing client certificate | Startup error | Unit: `TestInvalidTLSConfig`, `TestServeFailsOnInvalidBackendTLS` |

## CFG. Configuration

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| CFG-01 | A YAML file, including an empty one | Values from the file over the defaults | Unit: `TestLoadFile`, `TestLoadEmptyFileKeepsDefaults` |
| CFG-02 | `${VAR}` in the file | Replaced with the variable value, ignored in comments, an undefined variable is an error | Unit: `TestLoadFile`, `TestLoadFileErrors` |
| CFG-03 | A typo in a field, a wrong type, broken YAML, a missing file | Startup error | Unit: `TestLoadFileErrors` |
| CFG-04 | Invalid values: address, timeouts, templates, backend names and URLs, conflicting auth methods, TLS, headers | Validation error naming the field | Unit: `TestValidate` |
| CFG-05 | Several backends without `{site}` in the templates | An error; allowed with one backend | Unit: `TestValidate`, `TestConfigErrors` |
| CFG-06 | `-backend` and `-backend.header` flags, flags overriding the file | Explicitly set flags win over the file, `-backend` adds to the backends from the file, `-backend.header` applies to them too | Unit: `TestConfigFromFlags`, `TestFlagsOverrideConfigFile`, `TestParseBackend`, `TestParseBackendHeader`, `TestHostHeader` |
| CFG-07 | Invalid flags: a backend without a name, a header for an unknown site, a header without a value | Startup error | Unit: `TestConfigErrors` |
| CFG-08 | The `config.example.yaml` and `deploy/kubernetes/configmap.yaml` examples | Load and pass validation | Unit: `TestLoadExampleConfig`, `TestKubernetesExample` |
| CFG-09 | `-loggerLevel` and `-loggerFormat` | Valid values are accepted, others are an error | Unit: `TestNewLogger` |

## SRV. Service

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| SRV-01 | Start, serving requests, shutdown | The service responds, shuts down without errors and stops listening on the port | Unit: `TestServe` |
| SRV-02 | `/health`, `/-/healthy`, `/-/ready` | 200 `OK` | Unit: `TestServicePaths`, `TestServe` |
| SRV-03 | vmalert web UI (`/vmalert/`, `/vmalert/groups`, `/rules`) and unknown paths | 501 and 404 | Unit: `TestServicePaths`. E2E: "vmalert web UI is not supported" |
| SRV-04 | API paths with and without the `/vmalert` prefix | Handled the same way | Unit: `TestSingleRule`, `TestRulesPageNumPagination`. E2E: "vmui pagination" |
| SRV-05 | Methods other than GET and POST | 405, the request doesn't reach vmalert | Unit: `TestMethodNotAllowed` |
| SRV-06 | `/metrics` | `vmalertsproxy_backend_up`, request and error counters, durations, incoming requests; unknown paths share one `path` label value | Unit: `TestServicePaths`. E2E: "backend availability metrics" |

## BLD. Build

| ID | Scenario | Expected result | Automated tests |
|---|---|---|---|
| BLD-01 | `./build.sh -t <tag>` | Unit tests pass before the build, the version is in `-version` and in the image's OCI labels | Manual |
| BLD-02 | `./build.sh -p linux/amd64,linux/arm64` | The image is built for both platforms without emulation | Manual |
| BLD-03 | `./build.sh --e2e` | The test stand is removed after e2e, even if the tests fail | Manual |
| BLD-04 | An unknown option, an invalid tag, an option without a value | The script exits with an error and a hint | Manual |
