// Command vmalertsproxy merges vmalert APIs of multiple vmalert instances.
//
// It is designed to run as a vmselect sidecar set as -vmalert.proxyURL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vmalertsproxy/internal/config"
	"vmalertsproxy/internal/proxy"
)

// version is set at build time.
var version = "dev"

// options holds command-line flags.
type options struct {
	configFile     *string
	listenAddr     *string
	backendTimeout *time.Duration
	cacheTTL       *time.Duration
	allowPartial   *bool
	rewriteFile    *string
	rewriteGroup   *string
	siteLabel      *string
	labelRules     *bool
	labelAlerts    *bool
	logLevel       *string
	logFormat      *string
	showVersion    *bool
	backends       backendFlags
	backendHeaders backendFlags
}

func newOptions(fs *flag.FlagSet) *options {
	defaults := config.Default()
	o := &options{
		configFile:     fs.String("config", "", "Path to the YAML config file. Flags set explicitly override values from the file"),
		listenAddr:     fs.String("httpListenAddr", defaults.ListenAddr, "Address to listen for HTTP requests"),
		backendTimeout: fs.Duration("backend.timeout", defaults.BackendTimeout, "Timeout for a single request to a backend"),
		cacheTTL:       fs.Duration("cache.ttl", defaults.CacheTTL, "How long rules fetched from a backend are reused. Zero disables caching"),
		allowPartial:   fs.Bool("allowPartialResponse", defaults.AllowPartialResponse, "Return data of available backends when some of them fail"),
		rewriteFile:    fs.String("rewrite.file", defaults.Rewrite.File, "Template for the group file shown by Grafana as a namespace. Placeholders: {site}, {file}, {group}"),
		rewriteGroup:   fs.String("rewrite.group", defaults.Rewrite.Group, "Template for the group name. Placeholders: {site}, {file}, {group}"),
		siteLabel:      fs.String("rewrite.siteLabel", defaults.Rewrite.SiteLabel, "Label with the backend name. Empty value disables labeling"),
		labelRules:     fs.Bool("rewrite.labelRules", defaults.Rewrite.LabelRules, "Add -rewrite.siteLabel to rule labels"),
		labelAlerts: fs.Bool("rewrite.labelAlerts", defaults.Rewrite.LabelAlerts, "Add -rewrite.siteLabel to alert labels. "+
			"Grafana copies alert labels into silence matchers, so silences won't match alerts unless vmalert sends the same label"),
		logLevel:    fs.String("loggerLevel", "info", "Minimum log level: debug, info, warn or error"),
		logFormat:   fs.String("loggerFormat", "text", "Log format: text or json"),
		showVersion: fs.Bool("version", false, "Print version and exit"),
	}
	fs.Var(&o.backends, "backend", "Backend in the name=url form, e.g. msk=http://vmalert-msk:8880. Can be set multiple times")
	fs.Var(&o.backendHeaders, "backend.header", "Header for requests to the backend in the name=Header: value form, "+
		"e.g. 'msk=Host: vmalert.msk.example.com'. Applies to backends from both -backend and -config. Can be set multiple times")
	return o
}

type backendFlags []string

func (bf *backendFlags) String() string     { return strings.Join(*bf, ",") }
func (bf *backendFlags) Set(v string) error { *bf = append(*bf, v); return nil }

func main() {
	opts := newOptions(flag.CommandLine)
	flag.Parse()
	if *opts.showVersion {
		fmt.Println(version)
		return
	}
	log, err := newLogger(*opts.logLevel, *opts.logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(opts, log); err != nil {
		log.Error("fatal error", "err", err)
		os.Exit(1)
	}
}

func run(opts *options, log *slog.Logger) error {
	cfg, err := opts.config(flag.CommandLine)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, ln, log)
}

// serve serves the proxy on ln until ctx is done, then shuts the server down gracefully.
func serve(ctx context.Context, cfg *config.Config, ln net.Listener, log *slog.Logger) error {
	p, err := proxy.New(cfg, log)
	if err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	names := make([]string, 0, len(cfg.Backends))
	for _, b := range cfg.Backends {
		names = append(names, b.Name)
	}
	log.Info("vmalertsproxy started", "version", version, "addr", ln.Addr().String(), "backends", names)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// config returns the config from the -config file overridden by the flags explicitly set in fs.
func (o *options) config(fs *flag.FlagSet) (*config.Config, error) {
	cfg := config.Default()
	if *o.configFile != "" {
		if err := cfg.LoadFile(*o.configFile); err != nil {
			return nil, err
		}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "httpListenAddr":
			cfg.ListenAddr = *o.listenAddr
		case "backend.timeout":
			cfg.BackendTimeout = *o.backendTimeout
		case "cache.ttl":
			cfg.CacheTTL = *o.cacheTTL
		case "allowPartialResponse":
			cfg.AllowPartialResponse = *o.allowPartial
		case "rewrite.file":
			cfg.Rewrite.File = *o.rewriteFile
		case "rewrite.group":
			cfg.Rewrite.Group = *o.rewriteGroup
		case "rewrite.siteLabel":
			cfg.Rewrite.SiteLabel = *o.siteLabel
		case "rewrite.labelRules":
			cfg.Rewrite.LabelRules = *o.labelRules
		case "rewrite.labelAlerts":
			cfg.Rewrite.LabelAlerts = *o.labelAlerts
		}
	})
	for _, s := range o.backends {
		b, err := config.ParseBackend(s)
		if err != nil {
			return nil, err
		}
		cfg.Backends = append(cfg.Backends, b)
	}
	for _, s := range o.backendHeaders {
		if err := setBackendHeader(cfg, s); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func setBackendHeader(cfg *config.Config, s string) error {
	name, header, value, err := config.ParseBackendHeader(s)
	if err != nil {
		return err
	}
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.Name != name {
			continue
		}
		if b.Headers == nil {
			b.Headers = make(map[string]string)
		}
		b.Headers[header] = value
		return nil
	}
	return fmt.Errorf("-backend.header=%q refers to unknown backend %q", s, name)
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid -loggerLevel: %w", err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("invalid -loggerFormat %q: must be text or json", format)
	}
}
