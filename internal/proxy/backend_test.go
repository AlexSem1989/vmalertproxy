package proxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vmalertsproxy/internal/config"
)

func readRulesFixture(t *testing.T) []byte {
	t.Helper()
	rules, err := os.ReadFile("testdata/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestBackendFailuresArePartial(t *testing.T) {
	rules := readRulesFixture(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(rules)
	}))
	defer good.Close()

	for name, handler := range map[string]http.HandlerFunc{
		"http error": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		},
		"error status in body": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"status":"error","errorType":"internal","error":"boom"}`)
		},
		"truncated json": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"status":"success","data":{"groups":[`)
		},
		"html instead of json": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `<html>login page</html>`)
		},
		"unexpected schema": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"status":"success","data":{"groups":{"name":"node"}}}`)
		},
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := httptest.NewServer(handler)
			defer bad.Close()
			env := newEnvWithServers(t, map[string]string{"good": good.URL, "bad": bad.URL}, func(cfg *config.Config) {
				cfg.BackendTimeout = 300 * time.Millisecond
			})
			start := time.Now()
			resp := env.getRules(t, "/api/v1/rules", "")
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("a failed backend must not block the response for %s", elapsed)
			}
			if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], `"bad"`) {
				t.Fatalf("unexpected warnings %q", resp.Warnings)
			}
			if len(resp.Data.Groups) != 3 {
				t.Fatalf("groups of the available backend must be returned: %q", groupFiles(resp))
			}
			for _, g := range resp.Data.Groups {
				if !strings.HasPrefix(g.File, "good: ") {
					t.Fatalf("unexpected group %s", g.File)
				}
			}
		})
	}
}

func TestConcurrentRequestsShareBackendRequest(t *testing.T) {
	rules := readRulesFixture(t)
	var hits atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write(rules)
	}))
	defer srv.Close()
	env := newEnvWithServers(t, map[string]string{"msk": srv.URL}, nil)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if code, body := env.get(t, "/api/v1/rules", grafanaUserAgent); code != http.StatusOK {
				t.Errorf("unexpected status %d: %s", code, body)
			}
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Let the other requests join the in-flight backend request.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := hits.Load(); got != 1 {
		t.Fatalf("concurrent requests must share a single backend request, got %d", got)
	}
}

func TestCacheExpires(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, func(cfg *config.Config) {
		cfg.CacheTTL = 200 * time.Millisecond
	})
	env.getRules(t, "/api/v1/rules", "")
	env.getRules(t, "/api/v1/rules", "")
	if hits := env.backends["msk"].rulesHits.Load(); hits != 1 {
		t.Fatalf("expected a cached response, got %d backend requests", hits)
	}
	time.Sleep(300 * time.Millisecond)
	env.getRules(t, "/api/v1/rules", "")
	if hits := env.backends["msk"].rulesHits.Load(); hits != 2 {
		t.Fatalf("expected a new backend request after cache_ttl, got %d", hits)
	}
}

func TestFailuresAreNotCached(t *testing.T) {
	env := newTestEnv(t, []string{"msk"}, func(cfg *config.Config) {
		cfg.CacheTTL = time.Minute
	})
	env.backends["msk"].fail.Store(true)
	if code, _ := env.get(t, "/api/v1/rules", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", code)
	}
	env.backends["msk"].fail.Store(false)
	resp := env.getRules(t, "/api/v1/rules", "")
	if len(resp.Data.Groups) != 3 {
		t.Fatalf("a recovered backend must be requested again: %q", groupFiles(resp))
	}
}

func TestBearerToken(t *testing.T) {
	rules := readRulesFixture(t)
	var mu sync.Mutex
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write(rules)
	}))
	defer srv.Close()
	lastAuth := func() string {
		mu.Lock()
		defer mu.Unlock()
		return auth
	}

	inline := newEnvWithServers(t, map[string]string{"msk": srv.URL}, func(cfg *config.Config) {
		cfg.Backends[0].BearerToken = "token-1"
	})
	inline.getRules(t, "/api/v1/rules", "")
	if got := lastAuth(); got != "Bearer token-1" {
		t.Fatalf("unexpected Authorization %q", got)
	}

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile := newEnvWithServers(t, map[string]string{"msk": srv.URL}, func(cfg *config.Config) {
		cfg.Backends[0].BearerTokenFile = tokenFile
	})
	fromFile.getRules(t, "/api/v1/rules", "")
	if got := lastAuth(); got != "Bearer token-2" {
		t.Fatalf("unexpected Authorization %q", got)
	}
	// Rotated secrets are picked up without restart.
	if err := os.WriteFile(tokenFile, []byte("token-3"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile.getRules(t, "/api/v1/rules", "")
	if got := lastAuth(); got != "Bearer token-3" {
		t.Fatalf("rotated token must be used, got %q", got)
	}

	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	if code, _ := fromFile.get(t, "/api/v1/rules", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("a missing token file must fail the backend, got %d", code)
	}
}

func TestSecretsAreNotLogged(t *testing.T) {
	var logs bytes.Buffer
	cfg := config.Default()
	// Nothing listens on the port, so the request fails and the error is logged.
	cfg.Backends = []config.Backend{{Name: "msk", URL: "http://user:url-secret@127.0.0.1:1"}}
	p, err := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status %d", rec.Code)
	}
	if !strings.Contains(logs.String(), "cannot fetch from backend") {
		t.Fatalf("the failure must be logged: %s", logs.String())
	}
	for name, s := range map[string]string{"logs": logs.String(), "response": rec.Body.String()} {
		if strings.Contains(s, "url-secret") {
			t.Errorf("%s must not contain the password from the backend url: %s", name, s)
		}
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("responses must not expose backend addresses: %s", rec.Body.String())
	}
}

func TestInvalidTLSConfig(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tlsCfg := range map[string]config.TLS{
		"missing ca file":     {CAFile: filepath.Join(dir, "missing.crt")},
		"invalid ca file":     {CAFile: notPEM},
		"missing client cert": {CertFile: filepath.Join(dir, "missing.crt"), KeyFile: filepath.Join(dir, "missing.key")},
	} {
		cfg := config.Default()
		cfg.Backends = []config.Backend{{Name: "msk", URL: "https://vmalert:8880", TLS: tlsCfg}}
		if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestClientCertificate(t *testing.T) {
	rules := readRulesFixture(t)
	var peerCerts atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerCerts.Store(int64(len(r.TLS.PeerCertificates)))
		_, _ = w.Write(rules)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	// The test server certificate is reused as the client certificate.
	dir := t.TempDir()
	cert := srv.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, caFile := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	for path, data := range map[string][]byte{
		certFile: certPEM,
		keyFile:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		caFile:   certPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	env := newEnvWithServers(t, map[string]string{"msk": srv.URL}, func(cfg *config.Config) {
		cfg.Backends[0].TLS = config.TLS{CAFile: caFile, CertFile: certFile, KeyFile: keyFile}
	})
	env.getRules(t, "/api/v1/rules", "")
	if got := peerCerts.Load(); got != 1 {
		t.Fatalf("the client certificate must be sent, got %d certificates", got)
	}
}
