package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseOptions(t *testing.T, args ...string) (*options, *flag.FlagSet) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := newOptions(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return o, fs
}

func TestConfigFromFlags(t *testing.T) {
	o, fs := parseOptions(t,
		"-backend=msk=http://vmalert-msk:8880",
		"-backend=spb=https://10.0.0.1",
		"-backend.header=spb=Host: vmalert.spb.example.com",
		"-cache.ttl=0",
		"-rewrite.labelRules=false",
		"-rewrite.file=[{site}] {file}",
	)
	cfg, err := o.config(fs)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Backends) != 2 || cfg.Backends[1].HostHeader() != "vmalert.spb.example.com" {
		t.Fatalf("unexpected backends %+v", cfg.Backends)
	}
	if cfg.CacheTTL != 0 || cfg.Rewrite.LabelRules || cfg.Rewrite.File != "[{site}] {file}" {
		t.Fatalf("flags must be applied: %+v", cfg)
	}
	if cfg.ListenAddr != ":8880" || cfg.BackendTimeout != 10*time.Second || cfg.Rewrite.Group != "{group}" {
		t.Fatalf("unset flags must keep defaults: %+v", cfg)
	}
}

func TestFlagsOverrideConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
listen_addr: ":9999"
cache_ttl: 30s
backends:
  - name: msk
    url: http://vmalert-msk:8880
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	o, fs := parseOptions(t,
		"-config="+path,
		"-cache.ttl=1s",
		"-backend=spb=http://vmalert-spb:8880",
		"-backend.header=msk=X-Team: infra",
	)
	cfg, err := o.config(fs)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Fatalf("values from the file must be kept unless the flag is set, got %q", cfg.ListenAddr)
	}
	if cfg.CacheTTL != time.Second {
		t.Fatalf("explicitly set flags must override the file, got %v", cfg.CacheTTL)
	}
	if len(cfg.Backends) != 2 || cfg.Backends[0].Headers["X-Team"] != "infra" {
		t.Fatalf("-backend must extend backends from the file and -backend.header must apply to them: %+v", cfg.Backends)
	}
}

func TestConfigErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"no backends":            {},
		"backend without name":   {"-backend=http://vmalert:8880"},
		"header of unknown site": {"-backend=msk=http://vmalert:8880", "-backend.header=spb=Host: x"},
		"header without value":   {"-backend=msk=http://vmalert:8880", "-backend.header=msk=Host"},
		"missing config file":    {"-config=/nonexistent/config.yaml"},
		"no {site} in templates": {"-backend=a=http://a", "-backend=b=http://b", "-rewrite.file={file}"},
	} {
		o, fs := parseOptions(t, args...)
		if _, err := o.config(fs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNewLogger(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		if _, err := newLogger("debug", format); err != nil {
			t.Errorf("%s: unexpected error %s", format, err)
		}
	}
	if _, err := newLogger("verbose", "text"); err == nil {
		t.Error("expected an error for an invalid level")
	}
	if _, err := newLogger("info", "xml"); err == nil {
		t.Error("expected an error for an invalid format")
	}
}

func TestServe(t *testing.T) {
	vmalert := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"groups":[{"name":"g","file":"f.yml","rules":[]}]}}`)
	}))
	defer vmalert.Close()

	o, fs := parseOptions(t, "-backend=msk="+vmalert.URL)
	cfg, err := o.config(fs)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, ln, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	base := "http://" + ln.Addr().String()
	for path, want := range map[string]string{
		"/health":       "OK",
		"/api/v1/rules": `"file":"msk: f.yml"`,
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Fatalf("%s: unexpected response %d: %s", path, resp.StatusCode, body)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown must not fail: %s", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server didn't shut down")
	}
	if _, err := http.Get(base + "/health"); err == nil {
		t.Fatal("server must stop listening after shutdown")
	}
}

func TestServeFailsOnInvalidBackendTLS(t *testing.T) {
	o, fs := parseOptions(t, "-backend=msk=https://vmalert:8880")
	cfg, err := o.config(fs)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Backends[0].TLS.CAFile = "/nonexistent/ca.crt"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(context.Background(), cfg, ln, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected an error for a missing CA file")
	}
}
