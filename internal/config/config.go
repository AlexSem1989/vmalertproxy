// Package config describes the vmalertsproxy configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the proxy configuration.
type Config struct {
	// ListenAddr is the address to listen for HTTP requests.
	ListenAddr string `yaml:"listen_addr"`
	// BackendTimeout limits a single request to a backend.
	BackendTimeout time.Duration `yaml:"backend_timeout"`
	// CacheTTL is how long rules fetched from a backend are reused. Zero disables caching.
	CacheTTL time.Duration `yaml:"cache_ttl"`
	// AllowPartialResponse allows returning data of available backends when some of them fail.
	AllowPartialResponse bool `yaml:"allow_partial_response"`
	// Rewrite controls how objects of different backends are made distinguishable.
	Rewrite Rewrite `yaml:"rewrite"`
	// Backends is the list of vmalert instances.
	Backends []Backend `yaml:"backends"`
}

// Rewrite controls how objects of different backends are made distinguishable after merging.
type Rewrite struct {
	// File is the template for the group file, which Grafana shows as the rule namespace.
	// Supported placeholders: {site}, {file}, {group}.
	File string `yaml:"file"`
	// Group is the template for the group name. Supports the same placeholders as File.
	Group string `yaml:"group"`
	// SiteLabel is the label with the backend name. Empty value disables labeling.
	SiteLabel string `yaml:"site_label"`
	// LabelRules adds SiteLabel to rule labels.
	LabelRules bool `yaml:"label_rules"`
	// LabelAlerts adds SiteLabel to alert labels.
	LabelAlerts bool `yaml:"label_alerts"`
}

// Backend is a vmalert instance.
type Backend struct {
	// Name identifies the backend, e.g. the site name. It is used in templates and labels.
	Name string `yaml:"name"`
	// URL is the vmalert base URL, e.g. http://vmalert:8880.
	URL string `yaml:"url"`
	// Headers are added to every request to the backend.
	// Host overrides the Host header, e.g. for a backend behind an ingress accessed by IP.
	Headers         map[string]string `yaml:"headers"`
	BasicAuth       *BasicAuth        `yaml:"basic_auth"`
	BearerToken     string            `yaml:"bearer_token"`
	BearerTokenFile string            `yaml:"bearer_token_file"`
	TLS             TLS               `yaml:"tls"`
}

// BasicAuth holds basic auth credentials.
type BasicAuth struct {
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
}

// TLS holds TLS client settings.
type TLS struct {
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ServerName is used for SNI and certificate verification.
	// It defaults to the Host header if set, otherwise to the url host.
	ServerName string `yaml:"server_name"`
	// InsecureSkipVerify disables server certificate verification. CAFile is ignored then.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// HostHeader returns the Host header set in Headers.
func (b *Backend) HostHeader() string {
	for k, v := range b.Headers {
		if strings.EqualFold(k, "Host") {
			return v
		}
	}
	return ""
}

// Default returns the default configuration.
func Default() *Config {
	return &Config{
		ListenAddr:           ":8880",
		BackendTimeout:       10 * time.Second,
		CacheTTL:             5 * time.Second,
		AllowPartialResponse: true,
		Rewrite: Rewrite{
			File:        "{site}: {file}",
			Group:       "{group}",
			SiteLabel:   "vmalert_site",
			LabelRules:  true,
			LabelAlerts: false,
		},
	}
}

// LoadFile reads the YAML config file over the values already set in c.
//
// ${VAR} references are replaced with environment variable values.
func (c *Config) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	data, err = expandEnv(data)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("cannot parse %s: %w", path, err)
	}
	return nil
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references with environment variable values. Comment lines are skipped.
func expandEnv(data []byte) ([]byte, error) {
	var missing []string
	lines := bytes.SplitAfter(data, []byte("\n"))
	for i, line := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
			continue
		}
		lines[i] = envRefRe.ReplaceAllFunc(line, func(ref []byte) []byte {
			name := string(envRefRe.FindSubmatch(ref)[1])
			v, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return []byte(v)
		})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined environment variables: %s", strings.Join(missing, ", "))
	}
	return bytes.Join(lines, nil), nil
}

// ParseBackend parses the backend in the name=url form.
func ParseBackend(s string) (Backend, error) {
	name, u, ok := strings.Cut(s, "=")
	if !ok {
		return Backend{}, fmt.Errorf("backend %q must be in the name=url form", s)
	}
	return Backend{Name: name, URL: u}, nil
}

// ParseBackendHeader parses the backend header in the "name=Header: value" form.
func ParseBackendHeader(s string) (backend, header, value string, err error) {
	backend, line, ok := strings.Cut(s, "=")
	if !ok {
		return "", "", "", fmt.Errorf("backend header %q must be in the name=Header: value form", s)
	}
	header, value, ok = strings.Cut(line, ":")
	if !ok {
		return "", "", "", fmt.Errorf("backend header %q must be in the name=Header: value form", s)
	}
	return backend, strings.TrimSpace(header), strings.TrimSpace(value), nil
}

var (
	backendNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// headerNameRe matches the token chars allowed in header names by RFC 9110.
	headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// Validate checks the configuration.
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr must be set")
	}
	if c.BackendTimeout <= 0 {
		return errors.New("backend_timeout must be positive")
	}
	if c.CacheTTL < 0 {
		return errors.New("cache_ttl cannot be negative")
	}
	if c.Rewrite.File == "" || c.Rewrite.Group == "" {
		return errors.New("rewrite.file and rewrite.group cannot be empty")
	}
	if len(c.Backends) == 0 {
		return errors.New("at least one backend must be set")
	}
	if len(c.Backends) > 1 && !strings.Contains(c.Rewrite.File+c.Rewrite.Group, "{site}") {
		// Otherwise Grafana merges identical groups of different backends into one.
		return errors.New("rewrite.file or rewrite.group must contain {site} when multiple backends are set")
	}
	names := make(map[string]bool, len(c.Backends))
	for i := range c.Backends {
		b := &c.Backends[i]
		if err := b.validate(); err != nil {
			return fmt.Errorf("backend #%d (%s): %w", i+1, b.Name, err)
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate backend name %q", b.Name)
		}
		names[b.Name] = true
	}
	return nil
}

func (b *Backend) validate() error {
	if !backendNameRe.MatchString(b.Name) {
		return fmt.Errorf("name must match %s", backendNameRe)
	}
	u, err := url.Parse(b.URL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http or https url")
	}
	for k, v := range b.Headers {
		if !headerNameRe.MatchString(k) {
			return fmt.Errorf("invalid header name %q", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %q value cannot contain line breaks", k)
		}
		if strings.EqualFold(k, "Host") && v == "" {
			return errors.New("header Host cannot be empty")
		}
	}
	if ba := b.BasicAuth; ba != nil {
		if ba.Username == "" {
			return errors.New("basic_auth.username must be set")
		}
		if ba.Password != "" && ba.PasswordFile != "" {
			return errors.New("basic_auth.password and basic_auth.password_file are mutually exclusive")
		}
	}
	if b.BearerToken != "" && b.BearerTokenFile != "" {
		return errors.New("bearer_token and bearer_token_file are mutually exclusive")
	}
	if b.BasicAuth != nil && (b.BearerToken != "" || b.BearerTokenFile != "") {
		return errors.New("basic_auth and bearer token are mutually exclusive")
	}
	if (b.TLS.CertFile == "") != (b.TLS.KeyFile == "") {
		return errors.New("tls.cert_file and tls.key_file must be set together")
	}
	return nil
}
