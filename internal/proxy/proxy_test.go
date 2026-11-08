package proxy

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vmalertsproxy/internal/config"
)

const grafanaUserAgent = "Grafana/12.2.0"

const ruleFixture = `{"state":"firing","name":"HostDown","query":"up == 0","labels":{"severity":"critical"},
"health":"ok","type":"alerting","id":"2001","group_id":"1001","group_name":"node","file":"/etc/vmalert/rules/node.yml",
"updates":[{"time":"2026-10-21T10:00:00Z","samples":1}]}`

const groupFixture = `{"name":"node","file":"/etc/vmalert/rules/node.yml","id":"1001","type":"prometheus","states":{},
"rules":[{"name":"HostDown","type":"alerting","state":"firing","health":"ok","id":"2001","group_id":"1001",
"group_name":"node","file":"/etc/vmalert/rules/node.yml","labels":{"severity":"critical"}}]}`

const alertFixture = `{"state":"firing","name":"HostDown","value":"0","labels":{"alertname":"HostDown","instance":"node-1:9100"},
"id":"5001","rule_id":"2001","group_id":"1001","expression":"up == 0","source":"http://vmalert/vmalert/alert"}`

const notifiersFixture = `{"status":"success","data":{"notifiers":[{"kind":"static",
"targets":[{"address":"http://alertmanager:9093/api/v2/alerts","labels":{"__address__":"alertmanager:9093"},"lastError":""}]}]}}`

// fakeVMAlert serves the vmalert API from fixtures.
type fakeVMAlert struct {
	srv       *httptest.Server
	rulesHits atomic.Int64
	fail      atomic.Bool

	mu       sync.Mutex
	requests []*http.Request
}

func newFakeVMAlert(t *testing.T) *fakeVMAlert {
	t.Helper()
	rules, err := os.ReadFile("testdata/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeVMAlert{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(r.Context()))
		f.mu.Unlock()
		if f.fail.Load() {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/prefix") {
		case "/api/v1/rules":
			f.rulesHits.Add(1)
			_, _ = w.Write(rules)
		case "/api/v1/notifiers":
			_, _ = io.WriteString(w, notifiersFixture)
		case "/api/v1/rule":
			if r.FormValue("group_id") != "1001" || r.FormValue("rule_id") != "2001" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"rule not found","errorType":404}`)
				return
			}
			_, _ = io.WriteString(w, ruleFixture)
		case "/api/v1/group":
			if r.FormValue("group_id") != "1001" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"group not found","errorType":404}`)
				return
			}
			_, _ = io.WriteString(w, groupFixture)
		case "/api/v1/alert":
			if r.FormValue("group_id") != "1001" || r.FormValue("alert_id") != "5001" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"alert not found","errorType":404}`)
				return
			}
			_, _ = io.WriteString(w, alertFixture)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVMAlert) lastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

type testEnv struct {
	proxy    *Proxy
	backends map[string]*fakeVMAlert
}

// newTestEnv starts fake backends serving identical rules, like vmalerts of different sites with the same rule files.
func newTestEnv(t *testing.T, names []string, mutate func(*config.Config)) *testEnv {
	t.Helper()
	env := &testEnv{backends: make(map[string]*fakeVMAlert)}
	urls := make(map[string]string, len(names))
	for _, name := range names {
		f := newFakeVMAlert(t)
		env.backends[name] = f
		urls[name] = f.srv.URL
	}
	env.proxy = newTestProxy(t, names, urls, mutate)
	return env
}

// newTestProxy returns a proxy without cache for the backends with the given names and urls.
func newTestProxy(t *testing.T, names []string, urls map[string]string, mutate func(*config.Config)) *Proxy {
	t.Helper()
	cfg := config.Default()
	cfg.CacheTTL = 0
	for _, name := range names {
		cfg.Backends = append(cfg.Backends, config.Backend{Name: name, URL: urls[name]})
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (env *testEnv) get(t *testing.T, target, userAgent string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	rec := httptest.NewRecorder()
	env.proxy.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (env *testEnv) getRules(t *testing.T, target, userAgent string) *testRulesResponse {
	t.Helper()
	code, body := env.get(t, target, userAgent)
	if code != http.StatusOK {
		t.Fatalf("GET %s: unexpected status %d: %s", target, code, body)
	}
	var resp testRulesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("cannot parse response: %s; body: %s", err, body)
	}
	return &resp
}

type testRulesResponse struct {
	Status      string   `json:"status"`
	Page        int      `json:"page"`
	TotalPages  int      `json:"total_pages"`
	TotalGroups int      `json:"total_groups"`
	TotalRules  int      `json:"total_rules"`
	Warnings    []string `json:"warnings"`
	Data        struct {
		Groups         []testGroup `json:"groups"`
		GroupNextToken string      `json:"groupNextToken"`
	} `json:"data"`
}

type testGroup struct {
	Name   string            `json:"name"`
	File   string            `json:"file"`
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
	States map[string]int    `json:"states"`
	Rules  []testRule        `json:"rules"`
}

type testRule struct {
	Name       string            `json:"name"`
	State      string            `json:"state"`
	ID         string            `json:"id"`
	GroupID    string            `json:"group_id"`
	GroupName  string            `json:"group_name"`
	File       string            `json:"file"`
	Labels     map[string]string `json:"labels"`
	Alerts     []testAlert       `json:"alerts"`
	MaxUpdates *int              `json:"max_updates_entries"`
	Updates    []json.RawMessage `json:"updates"`
}

type testAlert struct {
	ID      string            `json:"id"`
	RuleID  string            `json:"rule_id"`
	GroupID string            `json:"group_id"`
	Labels  map[string]string `json:"labels"`
}

func groupFiles(resp *testRulesResponse) []string {
	var files []string
	for _, g := range resp.Data.Groups {
		files = append(files, g.File+"/"+g.Name)
	}
	return files
}

// ruleNames returns "group/rule" items, and "group/" for groups without rules.
func ruleNames(resp *testRulesResponse) []string {
	var names []string
	for _, g := range resp.Data.Groups {
		if len(g.Rules) == 0 {
			names = append(names, g.Name+"/")
		}
		for _, r := range g.Rules {
			names = append(names, g.Name+"/"+r.Name)
		}
	}
	return names
}

func TestRulesFromIdenticalBackendsStayDistinct(t *testing.T) {
	env := newTestEnv(t, []string{"spb", "msk"}, nil)
	resp := env.getRules(t, "/api/v1/rules", "")

	want := []string{
		"msk: /etc/vmalert/rules/empty.yml/empty",
		"msk: /etc/vmalert/rules/logs.yml/logs",
		"msk: /etc/vmalert/rules/node.yml/node",
		"spb: /etc/vmalert/rules/empty.yml/empty",
		"spb: /etc/vmalert/rules/logs.yml/logs",
		"spb: /etc/vmalert/rules/node.yml/node",
	}
	if got := groupFiles(resp); !slices.Equal(got, want) {
		t.Fatalf("unexpected groups\ngot:  %q\nwant: %q", got, want)
	}

	for _, site := range []string{"msk", "spb"} {
		var g testGroup
		for _, gr := range resp.Data.Groups {
			if gr.File == site+": /etc/vmalert/rules/node.yml" {
				g = gr
			}
		}
		if g.ID != site+":1001" {
			t.Errorf("%s: unexpected group id %q", site, g.ID)
		}
		if g.Labels["team"] != "infra" {
			t.Errorf("%s: group labels must be kept: %v", site, g.Labels)
		}
		if g.States["firing"] != 1 {
			t.Errorf("%s: unexpected group states %v", site, g.States)
		}
		r := g.Rules[0]
		if r.Name != "HostDown" || r.ID != site+":2001" || r.GroupID != site+":1001" {
			t.Errorf("%s: unexpected rule ids: %+v", site, r)
		}
		if r.File != g.File || r.GroupName != "node" {
			t.Errorf("%s: rule file and group must follow the group: %q, %q", site, r.File, r.GroupName)
		}
		if r.Labels["vmalert_site"] != site || r.Labels["severity"] != "critical" {
			t.Errorf("%s: unexpected rule labels %v", site, r.Labels)
		}
		if r.MaxUpdates == nil || *r.MaxUpdates != 20 {
			t.Errorf("%s: fields unknown to the proxy must be kept", site)
		}
		a := r.Alerts[0]
		if a.ID != site+":5001" || a.RuleID != site+":2001" || a.GroupID != site+":1001" {
			t.Errorf("%s: unexpected alert ids: %+v", site, a)
		}
		// Grafana copies alert labels into silence matchers, so they must stay as vmalert sends them.
		if _, ok := a.Labels["vmalert_site"]; ok {
			t.Errorf("%s: alert labels must not be changed by default: %v", site, a.Labels)
		}
	}
}

func TestRulesGrafanaRuleView(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)

	// Grafana sends both Prometheus and Mimir params to find a rule for the rule view page.
	q := url.Values{}
	for _, name := range []string{"file", "file[]"} {
		q.Set(name, "spb: /etc/vmalert/rules/node.yml")
	}
	for _, name := range []string{"rule_group", "rule_group[]"} {
		q.Set(name, "node")
	}
	for _, name := range []string{"rule_name", "rule_name[]"} {
		q.Set(name, "HostDown")
	}
	resp := env.getRules(t, "/api/v1/rules?"+q.Encode(), grafanaUserAgent)
	if len(resp.Data.Groups) != 1 || len(resp.Data.Groups[0].Rules) != 1 {
		t.Fatalf("expected a single rule, got %q", ruleNames(resp))
	}
	if r := resp.Data.Groups[0].Rules[0]; r.ID != "spb:2001" {
		t.Fatalf("unexpected rule %+v", r)
	}
}

func TestRulesGrafanaGetsPrometheusGroupsOnly(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	resp := env.getRules(t, "/api/v1/rules", grafanaUserAgent)
	for _, g := range resp.Data.Groups {
		if g.Name == "logs" {
			t.Fatalf("vlogs groups must not be returned to Grafana: %q", groupFiles(resp))
		}
	}
	if len(resp.Data.Groups) != 4 {
		t.Fatalf("unexpected groups %q", groupFiles(resp))
	}
}

func TestRulesTokenPagination(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	all := groupFiles(env.getRules(t, "/api/v1/rules", ""))

	var got []string
	token := ""
	for range 10 {
		q := url.Values{"group_limit": {"4"}}
		if token != "" {
			q.Set("group_next_token", token)
		}
		resp := env.getRules(t, "/api/v1/rules?"+q.Encode(), "")
		if len(resp.Data.Groups) > 4 {
			t.Fatalf("page exceeds group_limit: %q", groupFiles(resp))
		}
		got = append(got, groupFiles(resp)...)
		token = resp.Data.GroupNextToken
		if token == "" {
			break
		}
	}
	if !slices.Equal(got, all) {
		t.Fatalf("pages must cover all groups exactly once\ngot:  %q\nwant: %q", got, all)
	}

	if code, _ := env.get(t, "/api/v1/rules?group_limit=1&group_next_token=%25%25", ""); code != http.StatusBadRequest {
		t.Fatalf("invalid token must be rejected, got %d", code)
	}
}

func TestRulesPageNumPagination(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)

	resp := env.getRules(t, "/vmalert/api/v1/rules?group_limit=4&page_num=2", "")
	if resp.Page != 2 || resp.TotalPages != 2 || resp.TotalGroups != 6 || resp.TotalRules != 8 {
		t.Fatalf("unexpected page counters: %+v", resp)
	}
	want := []string{"spb: /etc/vmalert/rules/logs.yml/logs", "spb: /etc/vmalert/rules/node.yml/node"}
	if got := groupFiles(resp); !slices.Equal(got, want) {
		t.Fatalf("unexpected page\ngot:  %q\nwant: %q", got, want)
	}

	for _, query := range []string{"group_limit=4&page_num=3", "page_num=1", "group_limit=0"} {
		if code, body := env.get(t, "/api/v1/rules?"+query, ""); code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", query, code, body)
		}
	}
}

func TestRulesFilters(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, nil)
	f := func(query string, want ...string) {
		t.Helper()
		resp := env.getRules(t, "/api/v1/rules?"+query, "")
		if got := ruleNames(resp); !slices.Equal(got, want) {
			t.Errorf("%s: unexpected rules\ngot:  %q\nwant: %q", query, got, want)
		}
	}
	// vmalert keeps groups without rules in the response regardless of rule filters.
	f("type=alert", "empty/", "logs/ErrorsInLogs", "node/HostDown", "node/DiskFull")
	f("type=record", "empty/", "node/job:up:sum")
	f("state=firing", "empty/", "node/HostDown")
	f("state=error&extended_states=true", "empty/", "node/DiskFull")
	f("state=noData,firing&extended_states=1", "empty/", "logs/ErrorsInLogs", "node/HostDown", "node/job:up:sum")
	f("search=DISK", "empty/", "node/DiskFull")
	f("search=msk", "empty/", "logs/ErrorsInLogs", "node/HostDown", "node/job:up:sum", "node/DiskFull")
	f(`match[]={severity="critical"}`, "empty/", "node/HostDown")
	f(`match[]={severity=~"crit.*|warn.*"}`, "empty/", "node/HostDown", "node/DiskFull")
	f(`match[]={severity!="critical",vmalert_site="msk"}`, "empty/", "logs/ErrorsInLogs", "node/job:up:sum", "node/DiskFull")
	f("datasource_type=vlogs", "logs/ErrorsInLogs")
	f("rule_group[]=node&rule_name[]=DiskFull", "node/DiskFull")

	for _, query := range []string{"type=bad", "state=bad", "match[]=sum(x)", `match[]={a=~"("}`, "group_limit=-1"} {
		if code, body := env.get(t, "/api/v1/rules?"+query, ""); code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", query, code, body)
		}
	}
}

func TestRulesExcludeAlerts(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, nil)
	_, body := env.get(t, "/api/v1/rules?exclude_alerts=true", "")
	if strings.Contains(string(body), `"alerts"`) {
		t.Fatalf("alerts must be excluded: %s", body)
	}
}

func TestAlerts(t *testing.T) {
	env := newTestEnv(t, []string{"spb", "msk"}, nil)
	code, body := env.get(t, `/api/v1/alerts?match[]={instance="node-1:9100"}`, "")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", code, body)
	}
	var resp struct {
		Data struct {
			Alerts []testAlert `json:"alerts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range resp.Data.Alerts {
		ids = append(ids, a.ID)
	}
	if want := []string{"msk:5001", "spb:5001"}; !slices.Equal(ids, want) {
		t.Fatalf("unexpected alerts %q, want %q", ids, want)
	}

	q := url.Values{"file[]": {"spb: /etc/vmalert/rules/node.yml"}}
	_, body = env.get(t, "/api/v1/alerts?"+q.Encode(), "")
	if !strings.Contains(string(body), `"spb:5001"`) || strings.Contains(string(body), `"msk:5001"`) {
		t.Fatalf("unexpected filtered alerts: %s", body)
	}
}

func TestPartialResponse(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	env.backends["spb"].fail.Store(true)
	resp := env.getRules(t, "/api/v1/rules", "")
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], `"spb"`) {
		t.Fatalf("unexpected warnings %q", resp.Warnings)
	}
	for _, g := range resp.Data.Groups {
		if strings.HasPrefix(g.File, "spb") {
			t.Fatalf("unexpected group of the failed backend: %s", g.File)
		}
	}
	if len(resp.Data.Groups) != 3 {
		t.Fatalf("unexpected groups %q", groupFiles(resp))
	}

	env.backends["msk"].fail.Store(true)
	if code, body := env.get(t, "/api/v1/rules", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all backends fail, got %d: %s", code, body)
	}

	strict := newTestEnv(t, []string{"msk", "spb"}, func(cfg *config.Config) {
		cfg.AllowPartialResponse = false
	})
	strict.backends["spb"].fail.Store(true)
	if code, body := strict.get(t, "/api/v1/alerts", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with partial response disabled, got %d: %s", code, body)
	}
}

func TestRulesCache(t *testing.T) {
	cached := newTestEnv(t, []string{"msk", "spb"}, func(cfg *config.Config) {
		cfg.CacheTTL = time.Minute
	})
	cached.getRules(t, "/api/v1/rules", "")
	cached.getRules(t, "/api/v1/rules?type=alert", "")
	cached.get(t, "/api/v1/alerts", "")
	for name, b := range cached.backends {
		if hits := b.rulesHits.Load(); hits != 1 {
			t.Errorf("%s: expected a single backend request, got %d", name, hits)
		}
	}

	uncached := newTestEnv(t, []string{"msk"}, nil)
	uncached.getRules(t, "/api/v1/rules", "")
	uncached.getRules(t, "/api/v1/rules", "")
	if hits := uncached.backends["msk"].rulesHits.Load(); hits != 2 {
		t.Errorf("expected 2 backend requests without cache, got %d", hits)
	}
}

func TestSingleRule(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	code, body := env.get(t, "/vmalert/api/v1/rule?group_id=spb:1001&rule_id=spb:2001", "")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", code, body)
	}
	if req := env.backends["msk"].lastRequest(); req != nil {
		t.Fatalf("unexpected request to msk: %s", req.URL)
	}
	req := env.backends["spb"].lastRequest()
	if req.URL.Path != "/api/v1/rule" || req.FormValue("group_id") != "1001" || req.FormValue("rule_id") != "2001" {
		t.Fatalf("unexpected backend request %s", req.URL)
	}
	var r testRule
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.ID != "spb:2001" || r.GroupID != "spb:1001" || r.File != "spb: /etc/vmalert/rules/node.yml" || r.Labels["vmalert_site"] != "spb" {
		t.Fatalf("unexpected rule %+v", r)
	}
	if len(r.Updates) != 1 {
		t.Fatalf("rule updates must be kept: %s", body)
	}

	for target, want := range map[string]int{
		"/api/v1/rule?group_id=spb:1001&rule_id=spb:9":    http.StatusNotFound,
		"/api/v1/rule?group_id=1001&rule_id=2001":         http.StatusBadRequest,
		"/api/v1/rule?group_id=nsk:1001&rule_id=nsk:2001": http.StatusNotFound,
		"/api/v1/rule?group_id=msk:1001&rule_id=spb:2001": http.StatusBadRequest,
		"/api/v1/rule": http.StatusBadRequest,
	} {
		if code, body := env.get(t, target, ""); code != want {
			t.Errorf("%s: expected %d, got %d: %s", target, want, code, body)
		}
	}
}

func TestNotifiers(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	code, body := env.get(t, "/api/v1/notifiers", "")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", code, body)
	}
	var resp struct {
		Data struct {
			Notifiers []struct {
				Kind    string `json:"kind"`
				Targets []struct {
					Address string            `json:"address"`
					Labels  map[string]string `json:"labels"`
				} `json:"targets"`
			} `json:"notifiers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, n := range resp.Data.Notifiers {
		for _, tg := range n.Targets {
			sites = append(sites, tg.Labels["vmalert_site"])
		}
	}
	if want := []string{"msk", "spb"}; !slices.Equal(sites, want) {
		t.Fatalf("unexpected notifier targets %q: %s", sites, body)
	}
}

func TestServicePaths(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, nil)
	env.getRules(t, "/api/v1/rules", "")
	for target, want := range map[string]int{
		"/health":            http.StatusOK,
		"/-/ready":           http.StatusOK,
		"/vmalert/groups":    http.StatusNotImplemented,
		"/vmalert/":          http.StatusNotImplemented,
		"/rules":             http.StatusNotImplemented,
		"/api/v1/query":      http.StatusNotFound,
		"/vmalert/api/v1/xx": http.StatusNotFound,
	} {
		if code, body := env.get(t, target, ""); code != want {
			t.Errorf("%s: expected %d, got %d: %s", target, want, code, body)
		}
	}
	_, body := env.get(t, "/metrics", "")
	for _, metric := range []string{
		`vmalertsproxy_backend_up{backend="msk"} 1`,
		`vmalertsproxy_backend_requests_total{backend="msk"} 1`,
		`vmalertsproxy_backend_errors_total{backend="msk"} 0`,
		`vmalertsproxy_backend_request_duration_seconds_count{backend="msk"} 1`,
		`vmalertsproxy_http_requests_total{path="/api/v1/rules",code="200"} 1`,
		// Unknown paths share a single label value, so the number of series stays bounded.
		`vmalertsproxy_http_requests_total{path="other",code="404"} 2`,
		`vmalertsproxy_http_requests_total{path="other",code="501"} 3`,
	} {
		if !strings.Contains(string(body), metric) {
			t.Errorf("metrics must contain %s", metric)
		}
	}
}

func TestBackendURLAndAuth(t *testing.T) {
	var got *http.Request
	rules, err := os.ReadFile("testdata/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		_, _ = w.Write(rules)
	}))
	defer srv.Close()

	passwordFile := t.TempDir() + "/password"
	if err := os.WriteFile(passwordFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Backends = []config.Backend{{
		Name:      "msk",
		URL:       srv.URL + "/prefix/?tenant=1",
		Headers:   map[string]string{"X-Scope-OrgID": "team", "Host": "vmalert.msk.example.com"},
		BasicAuth: &config.BasicAuth{Username: "user", PasswordFile: passwordFile},
	}}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body)
	}
	if got.URL.Path != "/prefix/api/v1/rules" || got.URL.Query().Get("tenant") != "1" {
		t.Fatalf("unexpected backend url %s", got.URL)
	}
	if user, password, ok := got.BasicAuth(); !ok || user != "user" || password != "secret" {
		t.Fatalf("unexpected basic auth %q:%q", user, password)
	}
	if got.Header.Get("X-Scope-OrgID") != "team" {
		t.Fatalf("custom headers must be sent")
	}
	if got.Host != "vmalert.msk.example.com" {
		t.Fatalf("unexpected Host %q", got.Host)
	}
}

// TestBackendHostHeaderTLS checks a backend behind an ingress accessed by IP over HTTPS:
// the Host header is used for SNI and certificate verification.
func TestBackendHostHeaderTLS(t *testing.T) {
	rules, err := os.ReadFile("testdata/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var host, serverName string
	// The test server certificate is valid for example.com.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, serverName = r.Host, r.TLS.ServerName
		_, _ = w.Write(rules)
	}))
	defer srv.Close()
	caFile := t.TempDir() + "/ca.crt"
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Backends = []config.Backend{{
		Name:    "ingress",
		URL:     srv.URL,
		Headers: map[string]string{"Host": "example.com:443"},
		TLS:     config.TLS{CAFile: caFile},
	}}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body)
	}
	if host != "example.com:443" || serverName != "example.com" {
		t.Fatalf("unexpected Host %q and SNI %q", host, serverName)
	}

	// The CA file isn't needed when certificates aren't verified.
	cfg.Backends[0].TLS = config.TLS{CAFile: "/nonexistent/ca.crt", InsecureSkipVerify: true}
	p, err = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status with insecure_skip_verify %d: %s", rec.Code, rec.Body)
	}
}
