// Package proxy merges vmalert APIs of multiple vmalert instances into a single API.
//
// The proxy is designed to be set as -vmalert.proxyURL of vmselect, so Grafana
// shows rules and alerts of all vmalert instances via a single datasource.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"vmalertsproxy/internal/config"
	"vmalertsproxy/internal/vmalert"
)

// Prometheus API error types.
const (
	errBadData     = "bad_data"
	errNotFound    = "not_found"
	errInternal    = "internal"
	errUnavailable = "unavailable"
)

// Proxy is an http.Handler serving the merged vmalert API.
type Proxy struct {
	backends     []*backend
	byName       map[string]*backend
	rewriter     *rewriter
	timeout      time.Duration
	allowPartial bool
	log          *slog.Logger
	metrics      *metrics.Set
}

// New returns a proxy for the backends from cfg. cfg must be validated.
func New(cfg *config.Config, log *slog.Logger) (*Proxy, error) {
	p := &Proxy{
		byName:       make(map[string]*backend, len(cfg.Backends)),
		rewriter:     &rewriter{cfg: cfg.Rewrite},
		timeout:      cfg.BackendTimeout,
		allowPartial: cfg.AllowPartialResponse,
		log:          log,
		metrics:      metrics.NewSet(),
	}
	for _, bc := range cfg.Backends {
		b, err := newBackend(bc, cfg.BackendTimeout, cfg.CacheTTL, p.rewriter, p.metrics)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", bc.Name, err)
		}
		p.backends = append(p.backends, b)
		p.byName[b.name] = b
	}
	return p, nil
}

// ServeHTTP implements http.Handler.
//
// Paths are accepted both with and without the /vmalert prefix, as vmselect proxies both of them.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health", "/-/healthy", "/-/ready":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "OK")
		return
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		p.metrics.WritePrometheus(w)
		metrics.WriteProcessMetrics(w)
		return
	}

	endpoint := strings.TrimPrefix(r.URL.Path, "/vmalert")
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	defer func() {
		path := endpoint
		if !knownEndpoints[endpoint] {
			path = "other"
		}
		p.metrics.GetOrCreateCounter(fmt.Sprintf(`vmalertsproxy_http_requests_total{path=%q,code="%d"}`, path, sw.status)).Inc()
		p.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(sw, http.StatusMethodNotAllowed, errBadData, fmt.Sprintf("method %s is not allowed", r.Method))
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(sw, http.StatusBadRequest, errBadData, err.Error())
		return
	}
	// Grafana supports only Prometheus rules. vmselect enforces it in the same way,
	// but older vmselect versions don't.
	if strings.HasPrefix(r.UserAgent(), "Grafana") && r.Form.Get("datasource_type") == "" {
		r.Form.Set("datasource_type", "prometheus")
	}

	switch endpoint {
	case "/api/v1/rules":
		p.serveRules(sw, r)
	case "/api/v1/alerts":
		p.serveAlerts(sw, r)
	case "/api/v1/notifiers":
		p.serveNotifiers(sw, r)
	case "/api/v1/group", "/api/v1/rule", "/api/v1/alert":
		p.serveObject(sw, r, endpoint)
	default:
		if isUIPath(r.URL.Path) {
			http.Error(sw, "vmalertsproxy: vmalert web UI cannot be merged across backends; "+
				"use Grafana, vmui or the JSON API at /api/v1/rules and /api/v1/alerts", http.StatusNotImplemented)
			return
		}
		writeError(sw, http.StatusNotFound, errNotFound, fmt.Sprintf("unsupported path %q", r.URL.Path))
	}
}

var knownEndpoints = map[string]bool{
	"/api/v1/rules":     true,
	"/api/v1/alerts":    true,
	"/api/v1/notifiers": true,
	"/api/v1/group":     true,
	"/api/v1/rule":      true,
	"/api/v1/alert":     true,
}

func isUIPath(path string) bool {
	switch path {
	case "/", "/vmalert", "/rules", "/alerts", "/notifiers":
		return true
	}
	return strings.HasPrefix(path, "/vmalert/") && !strings.HasPrefix(path, "/vmalert/api/")
}

type rulesResponse struct {
	Status      string `json:"status"`
	Page        int    `json:"page,omitempty"`
	TotalPages  int    `json:"total_pages,omitempty"`
	TotalGroups int    `json:"total_groups,omitempty"`
	TotalRules  int    `json:"total_rules,omitempty"`
	Data        struct {
		Groups         []*vmalert.Group `json:"groups"`
		GroupNextToken string           `json:"groupNextToken,omitempty"`
	} `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
}

func (p *Proxy) serveRules(w http.ResponseWriter, r *http.Request) {
	rf, err := newRulesFilter(r.Form)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadData, err.Error())
		return
	}
	groups, warnings, err := p.collectGroups(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable, err.Error())
		return
	}
	page, err := rf.apply(groups)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadData, err.Error())
		return
	}
	resp := rulesResponse{
		Status:      "success",
		Page:        page.page,
		TotalPages:  page.totalPages,
		TotalGroups: page.totalGroups,
		TotalRules:  page.totalRules,
		Warnings:    warnings,
	}
	resp.Data.Groups = page.groups
	resp.Data.GroupNextToken = page.nextToken
	writeJSON(w, http.StatusOK, resp)
}

type alertsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Alerts []*vmalert.Alert `json:"alerts"`
	} `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
}

// serveAlerts serves /api/v1/alerts from the rules of backends, since rules contain their active alerts.
// It allows sharing the cached rules between both APIs.
func (p *Proxy) serveAlerts(w http.ResponseWriter, r *http.Request) {
	af, err := newAlertsFilter(r.Form)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadData, err.Error())
		return
	}
	groups, warnings, err := p.collectGroups(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable, err.Error())
		return
	}
	resp := alertsResponse{Status: "success", Warnings: warnings}
	resp.Data.Alerts = af.apply(groups)
	writeJSON(w, http.StatusOK, resp)
}

type notifiersResponse struct {
	Status string `json:"status"`
	Data   struct {
		Notifiers []*vmalert.Notifier `json:"notifiers"`
	} `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
}

func (p *Proxy) serveNotifiers(w http.ResponseWriter, r *http.Request) {
	results := fanOut(r.Context(), p.backends, (*backend).notifiers)
	warnings, err := checkResults(p, "notifiers", results)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable, err.Error())
		return
	}
	resp := notifiersResponse{Status: "success", Warnings: warnings}
	resp.Data.Notifiers = make([]*vmalert.Notifier, 0)
	for _, res := range results {
		resp.Data.Notifiers = append(resp.Data.Notifiers, res.value...)
	}
	writeJSON(w, http.StatusOK, resp)
}

// collectGroups returns sorted groups of all the backends.
func (p *Proxy) collectGroups(ctx context.Context) ([]*vmalert.Group, []string, error) {
	results := fanOut(ctx, p.backends, (*backend).rules)
	warnings, err := checkResults(p, "rules", results)
	if err != nil {
		return nil, nil, err
	}
	var groups []*vmalert.Group
	for _, res := range results {
		groups = append(groups, res.value...)
	}
	sortGroups(groups)
	return groups, warnings, nil
}

// idParams are the params of single object APIs, which hold ids made by composeID.
var idParams = []string{"group_id", "rule_id", "alert_id"}

// serveObject proxies requests for a single group, rule or alert to the backend owning the object.
func (p *Proxy) serveObject(w http.ResponseWriter, r *http.Request, endpoint string) {
	args := make(url.Values, len(r.Form))
	for k, vs := range r.Form {
		args[k] = vs
	}
	var b *backend
	for _, param := range idParams {
		v := r.Form.Get(param)
		if v == "" {
			continue
		}
		site, id, ok := parseID(v)
		if !ok {
			writeError(w, http.StatusBadRequest, errBadData, fmt.Sprintf("failed to read %q param: invalid id %q", param, v))
			return
		}
		if b != nil && b.name != site {
			writeError(w, http.StatusBadRequest, errBadData, "ids belong to different backends")
			return
		}
		if b = p.byName[site]; b == nil {
			writeError(w, http.StatusNotFound, errNotFound, fmt.Sprintf("unknown backend %q in %q param", site, param))
			return
		}
		args[param] = []string{id}
	}
	if b == nil {
		writeError(w, http.StatusBadRequest, errBadData, `"group_id" param is required`)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	status, body, err := b.get(ctx, endpoint, args)
	if err != nil {
		b.errors.Inc()
		p.log.Warn("cannot fetch object from backend", "backend", b.name, "path", endpoint, "err", err)
		writeError(w, http.StatusBadGateway, errUnavailable, fmt.Sprintf("backend %q is unavailable; see vmalertsproxy logs for details", b.name))
		return
	}
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	var obj any
	switch endpoint {
	case "/api/v1/group":
		var g vmalert.Group
		err = json.Unmarshal(body, &g)
		p.rewriter.group(b.name, &g)
		obj = g
	case "/api/v1/rule":
		var rule vmalert.Rule
		err = json.Unmarshal(body, &rule)
		p.rewriter.rule(b.name, &rule)
		obj = rule
	case "/api/v1/alert":
		var a vmalert.Alert
		err = json.Unmarshal(body, &a)
		p.rewriter.alert(b.name, &a)
		obj = a
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, errInternal, fmt.Sprintf("cannot parse response of backend %q: %s", b.name, err))
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

type result[T any] struct {
	backend *backend
	value   T
	err     error
}

// fanOut calls fetch for all the backends concurrently.
func fanOut[T any](ctx context.Context, backends []*backend, fetch func(*backend, context.Context) (T, error)) []result[T] {
	results := make([]result[T], len(backends))
	var wg sync.WaitGroup
	for i, b := range backends {
		wg.Go(func() {
			v, err := fetch(b, ctx)
			results[i] = result[T]{backend: b, value: v, err: err}
		})
	}
	wg.Wait()
	return results
}

// checkResults logs failed backends and returns warnings for the merged response.
//
// It returns an error if the merged response cannot be returned. Errors and warnings
// contain backend names only, since backend errors may expose internal addresses.
func checkResults[T any](p *Proxy, api string, results []result[T]) ([]string, error) {
	var failed, warnings []string
	for _, res := range results {
		if res.err == nil {
			continue
		}
		p.log.Warn("cannot fetch from backend", "backend", res.backend.name, "api", api, "err", res.err)
		failed = append(failed, res.backend.name)
		warnings = append(warnings, fmt.Sprintf("backend %q is unavailable; see vmalertsproxy logs for details", res.backend.name))
	}
	if len(failed) == 0 {
		return nil, nil
	}
	if len(failed) == len(results) || !p.allowPartial {
		return nil, fmt.Errorf("unavailable backends: %s; see vmalertsproxy logs for details", strings.Join(failed, ", "))
	}
	return warnings, nil
}

type errorResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, errorResponse{Status: "error", ErrorType: errType, Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		data, _ = json.Marshal(errorResponse{Status: "error", ErrorType: errInternal, Error: err.Error()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// statusWriter records the response status code for metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}
