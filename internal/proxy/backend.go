package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"golang.org/x/sync/singleflight"

	"vmalertsproxy/internal/config"
	"vmalertsproxy/internal/vmalert"
)

// maxResponseSize limits the size of a backend response.
const maxResponseSize = 256 << 20

// backend is a vmalert instance.
type backend struct {
	name     string
	cfg      config.Backend
	url      *url.URL
	client   *http.Client
	timeout  time.Duration
	cacheTTL time.Duration
	rewriter *rewriter

	flight singleflight.Group

	mu       sync.Mutex
	groups   []*vmalert.Group
	cachedAt time.Time

	up       atomic.Bool
	requests *metrics.Counter
	errors   *metrics.Counter
	duration *metrics.Histogram
}

func newBackend(cfg config.Backend, timeout, cacheTTL time.Duration, rw *rewriter, set *metrics.Set) (*backend, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := newTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	if host := cfg.HostHeader(); host != "" && tlsCfg.ServerName == "" {
		// A backend behind an ingress or a reverse proxy is usually routed by both Host and SNI.
		tlsCfg.ServerName = hostname(host)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.ResponseHeaderTimeout = timeout

	b := &backend{
		name:     cfg.Name,
		cfg:      cfg,
		url:      u,
		client:   &http.Client{Transport: tr},
		timeout:  timeout,
		cacheTTL: cacheTTL,
		rewriter: rw,
		requests: set.NewCounter(fmt.Sprintf(`vmalertsproxy_backend_requests_total{backend=%q}`, cfg.Name)),
		errors:   set.NewCounter(fmt.Sprintf(`vmalertsproxy_backend_errors_total{backend=%q}`, cfg.Name)),
		duration: set.NewHistogram(fmt.Sprintf(`vmalertsproxy_backend_request_duration_seconds{backend=%q}`, cfg.Name)),
	}
	set.NewGauge(fmt.Sprintf(`vmalertsproxy_backend_up{backend=%q}`, cfg.Name), func() float64 {
		if b.up.Load() {
			return 1
		}
		return 0
	})
	return b, nil
}

func newTLSConfig(cfg config.TLS) (*tls.Config, error) {
	tc := &tls.Config{
		ServerName:         cfg.ServerName,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	// The CA isn't used when server certificates aren't verified.
	if cfg.CAFile != "" && !cfg.InsecureSkipVerify {
		data, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read tls.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("no certificates found in tls.ca_file %s", cfg.CAFile)
		}
		tc.RootCAs = pool
	}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("cannot load tls client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

// rules returns the rewritten groups of the backend.
//
// Groups are cached for cacheTTL, and concurrent callers share a single backend request.
// The returned groups are shared, so they must not be modified.
func (b *backend) rules(ctx context.Context) ([]*vmalert.Group, error) {
	if groups, ok := b.cached(); ok {
		return groups, nil
	}
	ch := b.flight.DoChan("rules", func() (any, error) {
		// The result is shared between callers, so it must not be canceled by the first caller.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.timeout)
		defer cancel()
		data, err := fetchData[vmalert.RulesData](ctx, b, "/api/v1/rules")
		b.up.Store(err == nil)
		if err != nil {
			return nil, err
		}
		for _, g := range data.Groups {
			b.rewriter.group(b.name, g)
		}
		b.store(data.Groups)
		return data.Groups, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]*vmalert.Group), nil
	}
}

func (b *backend) cached() ([]*vmalert.Group, bool) {
	if b.cacheTTL <= 0 {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.groups == nil || time.Since(b.cachedAt) >= b.cacheTTL {
		return nil, false
	}
	return b.groups, true
}

func (b *backend) store(groups []*vmalert.Group) {
	if groups == nil {
		groups = []*vmalert.Group{}
	}
	b.mu.Lock()
	b.groups, b.cachedAt = groups, time.Now()
	b.mu.Unlock()
}

// notifiers returns the rewritten notifiers of the backend.
func (b *backend) notifiers(ctx context.Context) ([]*vmalert.Notifier, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	data, err := fetchData[vmalert.NotifiersData](ctx, b, "/api/v1/notifiers")
	if err != nil {
		return nil, err
	}
	for _, n := range data.Notifiers {
		b.rewriter.notifier(b.name, n)
	}
	return data.Notifiers, nil
}

// fetchData requests the list API of the backend and returns the data of the response.
func fetchData[T any](ctx context.Context, b *backend, path string) (T, error) {
	var resp vmalert.Response[T]
	err := func() error {
		status, body, err := b.get(ctx, path, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("unexpected status code %d; response: %s", status, truncate(body))
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("cannot parse response: %w", err)
		}
		if resp.Status != "success" {
			return fmt.Errorf("unexpected status %q in response; error: %s", resp.Status, resp.Error)
		}
		return nil
	}()
	if err != nil {
		b.errors.Inc()
		return resp.Data, fmt.Errorf("GET %s: %w", path, err)
	}
	return resp.Data, nil
}

// get performs a GET request to the given API path and returns the response status code and body.
func (b *backend) get(ctx context.Context, path string, args url.Values) (int, []byte, error) {
	u := *b.url
	u.Path = strings.TrimSuffix(b.url.Path, "/") + path
	u.RawPath = ""
	// Keep query args of the backend url, e.g. tenant or auth params.
	q := b.url.Query()
	for k, vs := range args {
		q[k] = vs
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	if err := b.authorize(req); err != nil {
		return 0, nil, err
	}
	b.requests.Inc()
	start := time.Now()
	defer b.duration.UpdateDuration(start)
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return 0, nil, fmt.Errorf("cannot read response: %w", err)
	}
	if len(body) > maxResponseSize {
		return 0, nil, fmt.Errorf("response exceeds %d bytes", maxResponseSize)
	}
	return resp.StatusCode, body, nil
}

// authorize sets auth headers. Secret files are read on every request, so rotated secrets are picked up.
func (b *backend) authorize(req *http.Request) error {
	for k, v := range b.cfg.Headers {
		// net/http ignores the Host header in req.Header and sends req.Host instead.
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if ba := b.cfg.BasicAuth; ba != nil {
		password := ba.Password
		if ba.PasswordFile != "" {
			data, err := os.ReadFile(ba.PasswordFile)
			if err != nil {
				return fmt.Errorf("cannot read basic_auth.password_file: %w", err)
			}
			password = strings.TrimSpace(string(data))
		}
		req.SetBasicAuth(ba.Username, password)
	}
	token := b.cfg.BearerToken
	if b.cfg.BearerTokenFile != "" {
		data, err := os.ReadFile(b.cfg.BearerTokenFile)
		if err != nil {
			return fmt.Errorf("cannot read bearer_token_file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// hostname returns host without port.
func hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func truncate(body []byte) []byte {
	const maxLen = 512
	if len(body) > maxLen {
		return body[:maxLen]
	}
	return body
}
