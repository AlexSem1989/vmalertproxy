package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"vmalertsproxy/internal/config"
)

func TestSingleGroup(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	code, body := env.get(t, "/vmalert/api/v1/group?group_id=msk:1001", "")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", code, body)
	}
	if req := env.backends["msk"].lastRequest(); req.URL.Path != "/api/v1/group" || req.FormValue("group_id") != "1001" {
		t.Fatalf("unexpected backend request %s", req.URL)
	}
	var g testGroup
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatal(err)
	}
	if g.ID != "msk:1001" || g.File != "msk: /etc/vmalert/rules/node.yml" || g.Name != "node" {
		t.Fatalf("unexpected group %+v", g)
	}
	r := g.Rules[0]
	if r.ID != "msk:2001" || r.GroupID != "msk:1001" || r.File != g.File || r.Labels["vmalert_site"] != "msk" {
		t.Fatalf("rules of the group must be rewritten: %+v", r)
	}
	if code, _ := env.get(t, "/api/v1/group?group_id=msk:9", ""); code != http.StatusNotFound {
		t.Fatalf("backend 404 must be passed through, got %d", code)
	}
}

func TestSingleAlert(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	code, body := env.get(t, "/vmalert/api/v1/alert?group_id=spb:1001&alert_id=spb:5001", "")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", code, body)
	}
	req := env.backends["spb"].lastRequest()
	if req.URL.Path != "/api/v1/alert" || req.FormValue("group_id") != "1001" || req.FormValue("alert_id") != "5001" {
		t.Fatalf("unexpected backend request %s", req.URL)
	}
	var a testAlert
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	if a.ID != "spb:5001" || a.RuleID != "spb:2001" || a.GroupID != "spb:1001" {
		t.Fatalf("unexpected alert %+v", a)
	}
	if _, ok := a.Labels["vmalert_site"]; ok {
		t.Fatalf("alert labels must not be changed by default: %v", a.Labels)
	}
	if !strings.Contains(string(body), `"source":"http://vmalert/vmalert/alert"`) {
		t.Fatalf("fields unknown to the proxy must be kept: %s", body)
	}
}

func TestSingleObjectBackendUnavailable(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	env.backends["spb"].srv.Close()
	code, body := env.get(t, "/api/v1/rule?group_id=spb:1001&rule_id=spb:2001", "")
	if code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", code, body)
	}
	if strings.Contains(string(body), "127.0.0.1") {
		t.Fatalf("errors must not expose backend addresses: %s", body)
	}
}

func TestSingleObjectMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id": 1`)
	}))
	defer srv.Close()
	env := newEnvWithServers(t, map[string]string{"msk": srv.URL}, nil)
	if code, body := env.get(t, "/api/v1/rule?group_id=msk:1&rule_id=msk:2", ""); code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", code, body)
	}
}

func TestNotifiersPartialResponse(t *testing.T) {
	env := newTestEnv(t, []string{"msk", "spb"}, nil)
	env.backends["msk"].fail.Store(true)
	code, body := env.get(t, "/api/v1/notifiers", "")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", code, body)
	}
	if !strings.Contains(string(body), `"vmalert_site":"spb"`) || !strings.Contains(string(body), `backend \"msk\" is unavailable`) {
		t.Fatalf("expected notifiers of spb and a warning about msk: %s", body)
	}
	env.backends["spb"].fail.Store(true)
	if code, _ := env.get(t, "/api/v1/notifiers", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all backends fail, got %d", code)
	}
}

func TestRulesPostForm(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, nil)
	form := url.Values{"rule_name[]": {"DiskFull"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	env.proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body)
	}
	var resp testRulesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// vmalert keeps groups without rules regardless of rule filters.
	if got := ruleNames(&resp); !slices.Equal(got, []string{"empty/", "node/DiskFull"}) {
		t.Fatalf("form params of POST requests must be applied, got %q", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, nil)
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		env.proxy.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/rules", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", method, rec.Code)
		}
	}
	if hits := env.backends["msk"].rulesHits.Load(); hits != 0 {
		t.Fatalf("rejected requests must not reach backends, got %d requests", hits)
	}
}

func TestGrafanaUserAgentKeepsExplicitDatasourceType(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, nil)
	resp := env.getRules(t, "/api/v1/rules?datasource_type=vlogs", grafanaUserAgent)
	if got := ruleNames(resp); !slices.Equal(got, []string{"logs/ErrorsInLogs"}) {
		t.Fatalf("explicit datasource_type must not be overridden, got %q", got)
	}
}

func TestErrorResponseFormat(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, nil)
	code, body := env.get(t, "/api/v1/rules?type=bad", "")
	if code != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", code)
	}
	var resp struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "error" || resp.ErrorType != "bad_data" || resp.Error == "" {
		t.Fatalf("errors must follow the Prometheus API format: %s", body)
	}
}

// TestPrometheusCompatibleBackend checks a backend returning the plain Prometheus rules API without vmalert fields.
func TestPrometheusCompatibleBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"groups":[{"name":"node","file":"node.yml","interval":30,
"rules":[{"name":"HostDown","query":"up == 0","type":"alerting","state":"firing","health":"ok","labels":{"severity":"critical"},
"alerts":[{"labels":{"alertname":"HostDown"},"state":"firing","value":"0"}]},
{"name":"up:sum","query":"sum(up)","type":"recording","health":"ok"}]}]}}`)
	}))
	defer srv.Close()
	env := newEnvWithServers(t, map[string]string{"prom": srv.URL}, nil)
	code, body := env.get(t, "/api/v1/rules", grafanaUserAgent)
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", code, body)
	}
	var resp struct {
		Data struct {
			Groups []map[string]json.RawMessage `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data.Groups) != 1 {
		t.Fatalf("groups without type must be treated as Prometheus groups: %s", body)
	}
	g := resp.Data.Groups[0]
	for _, key := range []string{"id", "states", "type"} {
		if _, ok := g[key]; ok {
			t.Errorf("the proxy must not add %q missing in the backend response: %s", key, body)
		}
	}
	if string(g["file"]) != `"prom: node.yml"` {
		t.Fatalf("unexpected file %s", g["file"])
	}
	if !strings.Contains(string(g["rules"]), `"vmalert_site":"prom"`) {
		t.Fatalf("rule labels must be added: %s", g["rules"])
	}
}

// newEnvWithServers returns a proxy for the given backend urls. Backends are sorted by name.
func newEnvWithServers(t *testing.T, urls map[string]string, mutate func(*config.Config)) *testEnv {
	t.Helper()
	names := make([]string, 0, len(urls))
	for name := range urls {
		names = append(names, name)
	}
	slices.Sort(names)
	return &testEnv{proxy: newTestProxy(t, names, urls, mutate)}
}
