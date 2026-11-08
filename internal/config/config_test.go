package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFile(t *testing.T) {
	t.Setenv("VMALERT_PASSWORD", "secret")
	path := writeConfig(t, `
# ${VMALERTSPROXY_UNDEFINED_VAR} in comments is ignored.
cache_ttl: 0s
rewrite:
  file: "[{site}] {file}"
backends:
  - name: msk
    url: http://vmalert-msk:8880
    basic_auth:
      username: vmalert
      password: "${VMALERT_PASSWORD}"
  - name: spb
    url: https://vmalert-spb:8880
`)
	cfg := Default()
	if err := cfg.LoadFile(path); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.CacheTTL != 0 || cfg.BackendTimeout != 10*time.Second {
		t.Fatalf("unexpected durations: %v, %v", cfg.CacheTTL, cfg.BackendTimeout)
	}
	if cfg.Rewrite.File != "[{site}] {file}" || cfg.Rewrite.Group != "{group}" || !cfg.Rewrite.LabelRules {
		t.Fatalf("unset fields must keep defaults: %+v", cfg.Rewrite)
	}
	if len(cfg.Backends) != 2 || cfg.Backends[0].BasicAuth.Password != "secret" {
		t.Fatalf("unexpected backends: %+v", cfg.Backends)
	}
}

func TestLoadEmptyFileKeepsDefaults(t *testing.T) {
	cfg := Default()
	if err := cfg.LoadFile(writeConfig(t, "# only comments\n")); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != Default().ListenAddr || cfg.CacheTTL != Default().CacheTTL {
		t.Fatalf("defaults must be kept: %+v", cfg)
	}
}

func TestLoadFileErrors(t *testing.T) {
	for name, data := range map[string]string{
		"unknown field":  "backend_timeot: 5s",
		"missing env":    "listen_addr: ${VMALERTSPROXY_UNDEFINED_VAR}",
		"bad duration":   "cache_ttl: soon",
		"invalid yaml":   "backends: [",
		"wrong type":     "backends: {name: msk}",
		"unknown nested": "backends:\n  - name: msk\n    urll: http://vmalert",
	} {
		if err := Default().LoadFile(writeConfig(t, data)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := Default().LoadFile("/nonexistent/config.yaml"); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestLoadExampleConfig(t *testing.T) {
	t.Setenv("VMALERT_SPB_PASSWORD", "secret")
	cfg := Default()
	if err := cfg.LoadFile("../../config.example.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !hasHostHeader(cfg) {
		t.Fatal("the example must show a backend with the Host header")
	}
}

// TestKubernetesExample checks config.yaml embedded into the ConfigMap of the Kubernetes example.
func TestKubernetesExample(t *testing.T) {
	t.Setenv("VMALERT_AAA_0_PASSWORD", "secret")
	data, err := os.ReadFile("../../deploy/kubernetes/configmap.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var configYAML string
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
		}
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if doc.Kind == "ConfigMap" {
			configYAML = doc.Data["config.yaml"]
		}
	}
	if configYAML == "" {
		t.Fatal("ConfigMap with config.yaml not found")
	}
	cfg := Default()
	if err := cfg.LoadFile(writeConfig(t, configYAML)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !hasHostHeader(cfg) {
		t.Fatal("the example must show a backend with the Host header")
	}
}

func hasHostHeader(cfg *Config) bool {
	for _, b := range cfg.Backends {
		if b.HostHeader() != "" {
			return true
		}
	}
	return false
}

func TestValidate(t *testing.T) {
	valid := func() *Config {
		cfg := Default()
		cfg.Backends = []Backend{
			{Name: "msk", URL: "http://vmalert-msk:8880"},
			{Name: "spb", URL: "http://vmalert-spb:8880"},
		}
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	f := func(wantErr string, mutate func(*Config)) {
		t.Helper()
		cfg := valid()
		mutate(cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("expected error containing %q, got %v", wantErr, err)
		}
	}
	f("listen_addr", func(c *Config) { c.ListenAddr = "" })
	f("backend_timeout", func(c *Config) { c.BackendTimeout = 0 })
	f("cache_ttl", func(c *Config) { c.CacheTTL = -time.Second })
	f("cannot be empty", func(c *Config) { c.Rewrite.Group = "" })
	f("at least one backend", func(c *Config) { c.Backends = nil })
	f("duplicate backend name", func(c *Config) { c.Backends[1].Name = "msk" })
	f("name must match", func(c *Config) { c.Backends[0].Name = "msk:1" })
	f("name must match", func(c *Config) { c.Backends[0].Name = "" })
	f("absolute http", func(c *Config) { c.Backends[0].URL = "vmalert:8880" })
	f("absolute http", func(c *Config) { c.Backends[0].URL = "ftp://vmalert" })
	f("must contain {site}", func(c *Config) { c.Rewrite.File = "{file}" })
	f("username", func(c *Config) { c.Backends[0].BasicAuth = &BasicAuth{Password: "p"} })
	f("password_file are mutually exclusive", func(c *Config) {
		c.Backends[0].BasicAuth = &BasicAuth{Username: "u", Password: "p", PasswordFile: "/p"}
	})
	f("bearer_token_file are mutually exclusive", func(c *Config) {
		c.Backends[0].BearerToken = "t"
		c.Backends[0].BearerTokenFile = "/t"
	})
	f("basic_auth and bearer token", func(c *Config) {
		c.Backends[0].BasicAuth = &BasicAuth{Username: "u"}
		c.Backends[0].BearerToken = "t"
	})
	f("cert_file and tls.key_file", func(c *Config) { c.Backends[0].TLS.CertFile = "cert.pem" })
	f("invalid header name", func(c *Config) { c.Backends[0].Headers = map[string]string{"X Bad": "v"} })
	f("line breaks", func(c *Config) { c.Backends[0].Headers = map[string]string{"X-Team": "a\r\nX-Injected: b"} })
	f("Host cannot be empty", func(c *Config) { c.Backends[0].Headers = map[string]string{"host": ""} })

	// {site} in the group template is enough to tell groups of different backends apart.
	groupSite := valid()
	groupSite.Rewrite.File = "{file}"
	groupSite.Rewrite.Group = "{site}/{group}"
	if err := groupSite.Validate(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	single := valid()
	single.Backends = single.Backends[:1]
	single.Rewrite.File = "{file}"
	if err := single.Validate(); err != nil {
		t.Fatalf("a single backend doesn't need {site} in templates: %s", err)
	}
}

func TestParseBackend(t *testing.T) {
	b, err := ParseBackend("msk=http://vmalert-msk:8880/?a=b")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "msk" || b.URL != "http://vmalert-msk:8880/?a=b" {
		t.Fatalf("unexpected backend %+v", b)
	}
	if _, err := ParseBackend("http://vmalert:8880"); err == nil {
		t.Fatal("expected an error for a backend without name")
	}
}

func TestParseBackendHeader(t *testing.T) {
	backend, header, value, err := ParseBackendHeader("msk=Host: vmalert.msk.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if backend != "msk" || header != "Host" || value != "vmalert.msk.example.com" {
		t.Fatalf("unexpected header %q %q %q", backend, header, value)
	}
	if _, _, value, _ := ParseBackendHeader("msk=X-Query: a=b:c"); value != "a=b:c" {
		t.Fatalf("value must keep = and : chars, got %q", value)
	}
	for _, s := range []string{"Host: vmalert", "msk=Host"} {
		if _, _, _, err := ParseBackendHeader(s); err == nil {
			t.Errorf("%q: expected an error", s)
		}
	}
}

func TestHostHeader(t *testing.T) {
	b := Backend{Headers: map[string]string{"X-Team": "a", "host": "vmalert.msk.example.com"}}
	if got := b.HostHeader(); got != "vmalert.msk.example.com" {
		t.Fatalf("unexpected Host %q", got)
	}
	if got := (&Backend{}).HostHeader(); got != "" {
		t.Fatalf("unexpected Host %q", got)
	}
}
