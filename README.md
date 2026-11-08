# vmalertsproxy

See the rules and alerts of all your vmalert instances in Grafana, through a single VictoriaMetrics datasource.

VictoriaMetrics can show vmalert rules in Grafana: you set
[`-vmalert.proxyURL`](https://docs.victoriametrics.com/victoriametrics/vmalert/#grafana) on vmselect (or on
single-node VictoriaMetrics), and Grafana's Alerting page gets the rules from that vmalert. The catch is that the flag
takes exactly one URL. If you run a vmalert per site, per team or per cluster, Grafana only ever sees one of them.

vmalertsproxy is a small sidecar that takes the place of that one URL. It asks every vmalert you configure and
returns a single merged response in the vmalert/Prometheus format, which Grafana and vmui already understand.

```
Grafana ──> vmselect ──(-vmalert.proxyURL)──> vmalertsproxy ──┬──> vmalert msk
                                                              ├──> vmalert spb
                                                              └──> vmalert nsk
```

## Why this exists

The limitation is tracked upstream in
[VictoriaMetrics#11360](https://github.com/VictoriaMetrics/VictoriaMetrics/issues/11360): "allow
`-vmalert.proxyURL` to accept multiple vmalert urls". Two pull requests try to solve it inside vmselect,
[#11361](https://github.com/VictoriaMetrics/VictoriaMetrics/pull/11361) and
[#11488](https://github.com/VictoriaMetrics/VictoriaMetrics/pull/11488), but neither is merged at the time of
writing. Until one of them lands, stock VictoriaMetrics can't show several vmalerts in one datasource.

Fetching several responses and gluing them together is the easy part. The hard part is that the same rule files are
usually deployed to many vmalert instances. Then everything about a rule is identical across sites: the `file`, the
group name, the rule name, the expression, the labels, and even the IDs, because vmalert derives IDs from a hash of
the config. Grafana groups
[data source-managed rules](https://grafana.com/docs/grafana/latest/alerting/alerting-rules/create-data-source-managed-rule/)
by namespace (the `file` field) and group, so with a plain concatenation the `HostDown` rule of msk and the
`HostDown` rule of spb collapse into one rule in the UI.

They aren't duplicates. They are different alerts of different sites, and you want to see both. That's what
vmalertsproxy takes care of.

## How sites are kept apart

The proxy rewrites what each vmalert returns, so the rules of every site stay distinct after the merge:

| What | How | Why |
|---|---|---|
| Group `file` | `{site}: {file}`, e.g. `msk: /etc/vmalert/rules.yml` | Grafana shows `file` as the namespace, so every site gets its own namespace |
| Rule labels | `vmalert_site=<site>` is added | You can filter the Grafana rule list with `label:vmalert_site=msk` |
| Group, rule and alert IDs | `<site>:<id>` | IDs stay unique after the merge, and requests like `/api/v1/rule?group_id=…` reach the right vmalert |
| Alert labels | **left unchanged** | Grafana copies alert instance labels into silence matchers. A label that only the proxy adds would make the silence miss the real alert in Alertmanager |

Labels that already exist are never overwritten. Templates and the label name are configurable, see
[Configuration](#configuration).

> Alertmanager deduplicates identical alerts too. If you want alerts of different sites to be told apart there as
> well, start every vmalert with `-external.label=vmalert_site=<site>`. Then the real alerts carry the label, and
> silences created in Grafana work per site.

## Try it

The test stand starts Grafana and two vmalert instances that load the same rules. Only Docker is needed:

```bash
docker compose -f test/docker-compose.yml --profile grafana up -d --build
```

Open http://localhost:3000/alerting/list. Under Data source-managed rules you should see the `demo` group twice, in
the `msk: /etc/vmalert/rules.yml` and `spb: /etc/vmalert/rules.yml` namespaces. When you're done:

```bash
docker compose -f test/docker-compose.yml --profile grafana down -v
```

To run the proxy against your own vmalerts, build the image and give it their addresses:

```bash
./build.sh -t 0.1.0
```

```bash
docker run --rm -p 8880:8880 vmalertsproxy:0.1.0 -backend=msk=http://vmalert-msk:8880 -backend=spb=http://vmalert-spb:8880
```

Then point vmselect at it with `-vmalert.proxyURL=http://<proxy>:8880`. In production the proxy usually runs as a
[sidecar of vmselect](#running-next-to-vmselect).

## Grafana compatibility

Tested with Grafana 13.2 and a Prometheus datasource with `manageAlerts: true`, see [end-to-end tests](#tests).

- **Rule list** (Alerting → Alert rules → Data source-managed): every site shows up in its own namespace.
- **Pagination in Grafana 12+.** Grafana asks for pages with `group_limit` and `group_next_token`, like
  Prometheus 3 does. vmalert doesn't support tokens and returns only the first page, so the proxy paginates the
  merged list itself and returns `groupNextToken`.
- **Rule details page.** Grafana looks a rule up by `file[]`/`file`, `rule_group[]`/`rule_group` and
  `rule_name[]`/`rule_name`. Filters are applied to the rewritten values, so the page opens the rule of the right
  site.
- **Prometheus rules only.** Requests with `User-Agent: Grafana*` get `datasource_type=prometheus`, so Grafana
  doesn't receive VictoriaLogs or Graphite groups. Recent vmselect versions do the same, older ones don't.
- **Read-only.** vmalert has no ruler API, so rules can't be edited from Grafana. That's the same as without the
  proxy.

## API

Paths work with and without the `/vmalert` prefix, since vmselect proxies both.

| Path | What it does |
|---|---|
| `/api/v1/rules` | Groups of all vmalerts. Supports the vmalert filters `rule_group[]`, `file[]`, `rule_name[]`, `type`, `state`/`filter` (including the Grafana aliases `noData`, `normal`, `error`), `match[]`, `search`, `datasource_type`, `exclude_alerts` and `extended_states`. Paginates with `group_limit` + `group_next_token` (Grafana) or `group_limit` + `page_num` (vmui) |
| `/api/v1/alerts` | Alerts of all vmalerts, filtered by `rule_group[]`, `file[]`, `datasource_type` and `match[]`. Built from the same data as `/api/v1/rules`, so it costs no extra request to vmalert |
| `/api/v1/notifiers` | Notifiers of all vmalerts. The site label is added to target labels |
| `/api/v1/group`, `/api/v1/rule`, `/api/v1/alert` | Sent only to the vmalert that owns the ID (`group_id=msk:123`). IDs in the response are rewritten |
| `/vmalert/*` (vmalert web UI) | `501`: HTML pages can't be merged, use Grafana or vmui instead |
| `/health`, `/-/healthy`, `/-/ready` | `200 OK` |
| `/metrics` | Proxy metrics, see [Monitoring](#monitoring) |

Filters run in the proxy on the merged list instead of being passed to vmalert. That way they see the rewritten
`file` values, and pagination covers the groups of all sites.

## When a vmalert is down

- **Partial responses.** If some vmalerts don't answer, you get the rules of the others, and the `warnings` field
  lists the backends that failed. If all of them fail, or `allow_partial_response` is off, the proxy returns `503`.
- **Timeouts.** Every request to vmalert is limited by `backend_timeout`, so one stuck vmalert can't hang Grafana.
- **Cache.** The `/api/v1/rules` response of each vmalert is reused for `cache_ttl` (5s by default), and concurrent
  requests share one request to vmalert. Grafana polls rules from every open Alerting page, and the cache keeps
  those bursts away from vmalert. Failed responses are never cached.
- **No leaks.** Clients only see backend names, never addresses or error texts. The details go to the log.

## Configuration

The simplest setup needs only flags:

```bash
vmalertsproxy -backend=msk=http://vmalert-msk:8880 -backend=spb=http://vmalert-spb:8880
```

For authentication, TLS or custom headers use a YAML file (`-config=config.yaml`), see
[config.example.yaml](config.example.yaml). The file can reference environment variables as `${VAR}`. Flags you set
explicitly override the file, and `-backend` adds backends to the ones from the file.

| Flag | YAML | Default | Description |
|---|---|---|---|
| `-config` | | | Path to the YAML config |
| `-backend` | `backends` | | `name=url`, repeatable. The name must match `[A-Za-z0-9][A-Za-z0-9_.-]*` |
| `-backend.header` | `backends[].headers` | | Header sent to a backend, `'name=Header: value'`, repeatable. Works for backends from `-config` too |
| `-httpListenAddr` | `listen_addr` | `:8880` | Address to listen on |
| `-backend.timeout` | `backend_timeout` | `10s` | Timeout of a request to vmalert |
| `-cache.ttl` | `cache_ttl` | `5s` | How long rules of a vmalert are cached, `0` disables the cache |
| `-allowPartialResponse` | `allow_partial_response` | `true` | Return the rules of available vmalerts when some of them fail |
| `-rewrite.file` | `rewrite.file` | `{site}: {file}` | Template of the group `file`, which is the namespace in Grafana. Placeholders: `{site}`, `{file}`, `{group}` |
| `-rewrite.group` | `rewrite.group` | `{group}` | Template of the group name |
| `-rewrite.siteLabel` | `rewrite.site_label` | `vmalert_site` | Label with the site name, an empty value disables labeling |
| `-rewrite.labelRules` | `rewrite.label_rules` | `true` | Add the site label to rules |
| `-rewrite.labelAlerts` | `rewrite.label_alerts` | `false` | Add the site label to alerts. Enable it only if vmalert sends the same label to Alertmanager |
| `-loggerLevel` | | `info` | `debug`, `info`, `warn` or `error` |
| `-loggerFormat` | | `text` | `text` or `json` |

With more than one backend, `{site}` must appear in `rewrite.file` or `rewrite.group`. Otherwise identical rules
would collapse again, so the proxy refuses to start.

### vmalert behind an ingress: the Host header

Sometimes vmalert is reachable only through an ingress or a reverse proxy by IP, and the ingress picks the backend by
the `Host` header. Set `Host` in the backend headers. This is the equivalent of
`curl https://10.1.1.1/api/v1/rules -H 'Host: vmalert.mycluster.local'`:

```yaml
backends:
  - name: aaa-0
    url: https://10.1.1.1
    headers:
      Host: vmalert.mycluster.local
    tls:
      ca_file: /etc/ssl/internal/ca.crt
```

The same with flags:

```bash
vmalertsproxy -backend=aaa-0=https://10.1.1.1 '-backend.header=aaa-0=Host: vmalert.mycluster.local'
```

Over HTTPS, the name from `Host` is also used as SNI and to verify the certificate. Ingresses usually pick a
certificate by SNI, and the certificate is issued for the name, not for the IP. Note that curl behaves differently:
with `-H Host` it doesn't change SNI and doesn't send SNI for an IP at all. Set `tls.server_name` to override the
name.

If the certificate is signed by an internal CA, set `tls.ca_file`. `tls.insecure_skip_verify: true` turns off
verification, like `curl -k`. TLS settings are available only in the YAML file.

## Running next to vmselect

The usual setup is a sidecar in every vmselect pod, with vmselect talking to it over localhost:
`-vmalert.proxyURL=http://127.0.0.1:8880`. Each vmselect replica gets its own proxy and its own cache.

### VictoriaMetrics Operator (VMCluster)

```yaml
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata:
  name: vm
spec:
  vmselect:
    extraArgs:
      vmalert.proxyURL: http://127.0.0.1:8880
    containers:
      - name: vmalertsproxy
        image: registry.example.com/vmalertsproxy:0.1.0
        args:
          - -config=/etc/vmalertsproxy/config.yaml
        env:
          - name: VMALERT_AAA_0_PASSWORD
            valueFrom:
              secretKeyRef:
                name: vmalertsproxy
                key: aaa-0-password
        ports:
          - name: proxy
            containerPort: 8880
        readinessProbe:
          httpGet:
            path: /-/ready
            port: proxy
        livenessProbe:
          httpGet:
            path: /-/healthy
            port: proxy
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
          limits:
            memory: 256Mi
        volumeMounts:
          - name: vmalertsproxy-config
            mountPath: /etc/vmalertsproxy
            readOnly: true
    volumes:
      - name: vmalertsproxy-config
        configMap:
          name: vmalertsproxy
```

An example ConfigMap with the config and a CA certificate, plus a Secret with passwords, is in
[deploy/kubernetes/configmap.yaml](deploy/kubernetes/configmap.yaml). The config is read only at startup, so restart
the vmselect pods after changing the ConfigMap. Passwords and tokens from files (`password_file`,
`bearer_token_file`) are re-read on every request, so rotating them needs no restart.

### victoria-metrics-cluster Helm chart

```yaml
vmselect:
  extraArgs:
    vmalert.proxyURL: http://127.0.0.1:8880
  extraContainers:
    - name: vmalertsproxy
      image: registry.example.com/vmalertsproxy:0.1.0
      args:
        - -backend=msk=http://vmalert-msk.monitoring.svc:8880
        - -backend=spb=http://vmalert-spb.monitoring.svc:8880
      ports:
        - name: proxy
          containerPort: 8880
      readinessProbe:
        httpGet:
          path: /-/ready
          port: proxy
```

Single-node VictoriaMetrics has the same `-vmalert.proxyURL` flag, and the proxy works with it as well.

## Monitoring

| Metric | Description |
|---|---|
| `vmalertsproxy_backend_up{backend}` | `1` if the last rules request to the vmalert succeeded |
| `vmalertsproxy_backend_requests_total{backend}` | Requests to vmalert |
| `vmalertsproxy_backend_errors_total{backend}` | Failed requests to vmalert |
| `vmalertsproxy_backend_request_duration_seconds{backend}` | Duration of requests to vmalert (VictoriaMetrics histogram) |
| `vmalertsproxy_http_requests_total{path,code}` | Incoming requests |

It's worth alerting on `vmalertsproxy_backend_up == 0`: while a backend is down, its rules silently disappear from
Grafana.

## Alternatives

- **[VictoriaMetrics#11361](https://github.com/VictoriaMetrics/VictoriaMetrics/pull/11361)** lets vmselect take
  several URLs in `-vmalert.proxyURL`. It is open and not merged at the time of writing. It concatenates responses as
  they are, so identical rules of different sites still collapse in Grafana. Drill-down and notifier requests go to
  the first vmalert only, and Grafana 12 token pagination isn't supported.
- **[VictoriaMetrics#11488](https://github.com/VictoriaMetrics/VictoriaMetrics/pull/11488)** is a draft by the
  VictoriaMetrics team for the same issue. It makes `-vmalert.proxyURL` repeatable, adds `-vmalert.proxyName` for
  readable source names, and routes requests for a single group, rule or alert by a `vmalert_source` query arg. Keep
  an eye on it: once native support lands, you may not need this proxy.
- **One datasource per site**, the workaround suggested in
  [VictoriaMetrics#11360](https://github.com/VictoriaMetrics/VictoriaMetrics/issues/11360). Every Grafana
  datasource sends its own HTTP header, and `-vmalert.proxyURL` points at vmauth, which
  [routes by header](https://docs.victoriametrics.com/victoriametrics/vmauth/#routing-by-header) to the right
  vmalert. Sites are fully isolated, but you have to add and maintain a datasource for every site.

## Development

You only need Docker: tests and builds run in containers.

### Building the image

[build.sh](build.sh) runs the unit tests and builds the image. The version comes from `-t`, `$VERSION`,
`git describe` or falls back to `dev`. It is baked into the binary (`vmalertsproxy -version`) and into the image's OCI
labels.

```bash
./build.sh
```

An image for an amd64 cluster, built on an Apple Silicon Mac:

```bash
./build.sh -t 0.1.0 -p linux/amd64
```

A multi-arch image (amd64 + arm64), pushed to a registry:

```bash
./build.sh -i registry.example.com/monitoring/vmalertsproxy -t 0.1.0 --push --latest
```

There's no emulation involved: Go cross-compiles on the host platform. `--e2e` also runs the end-to-end tests, and
`--skip-tests` skips all tests. See `./build.sh --help` for the rest.

### Tests

All tests are described in [test/docker-compose.yml](test/docker-compose.yml).
[test/TEST_CASES.md](test/TEST_CASES.md) lists the test cases and the automated tests that cover each of them.

Unit tests (gofmt, go vet and `go test -race`):

```bash
docker compose -f test/docker-compose.yml run --rm unit
```

The end-to-end tests start VictoriaMetrics with `-vmalert.proxyURL` pointing at vmalertsproxy, and two vmalert
instances with the same rules ([test/rules.yml](test/rules.yml)). [test/e2e.sh](test/e2e.sh) then sends requests the
way Grafana does and checks:
- that sites are split into namespaces and get their labels and IDs;
- the rule details page, and Grafana and vmui pagination;
- drill-down, and partial responses when a vmalert is down;
- access through vmauth acting as an ingress: HTTPS with routing by `Host`, with a certificate issued only for the
  `Host` names ([test/vmalertsproxy-host.yaml](test/vmalertsproxy-host.yaml));
- metrics.

```bash
docker compose -f test/docker-compose.yml run --rm --build e2e
```

```bash
docker compose -f test/docker-compose.yml down
```

`VM_VERSION` and `GRAFANA_VERSION` pick the VictoriaMetrics and Grafana versions (`latest` by default), for example
`VM_VERSION=v1.120.0 docker compose -f test/docker-compose.yml run --rm e2e`.

The [Grafana stand](#try-it) has two datasources:
- **VictoriaMetrics** shows the msk and spb test sites.
- **VictoriaMetrics host** shows the backends from [test/vmalertsproxy-host.yaml](test/vmalertsproxy-host.yaml).
  You can add real vmalerts there, for example ones behind an ingress with `Host`, and browse their alerts in
  Grafana. This VictoriaMetrics instance is also available at http://localhost:8429.
