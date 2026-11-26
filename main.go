// Command llmgate is the one way from web pages to a local model: it serves
// apps (lwdescribe first) behind a reverse proxy, queues their requests through
// one line to the model server, and logs operations and a review record a
// request. Config: /usr/local/etc/llmgate/llmgate.toml, then LLMGATE_
// variables (LLMGATE_MODEL_THREADS=4). -check loads everything, prints, exits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/lixenwraith/config"
	"github.com/lixenwraith/llmgate/apps/lwdescribe"
	"github.com/lixenwraith/llmgate/gate"
	"github.com/lixenwraith/llmgate/model"
	"github.com/lixenwraith/log"
	"github.com/lixenwraith/toml"
)

// Config is llmgate.toml. Listen is where nginx reaches the apps, Admin a
// status page for the jail alone ("" for none), Proxies the addresses whose
// X-Real-IP is believed.
type Config struct {
	Listen  string   `toml:"listen"`
	Admin   string   `toml:"admin"`
	Proxies []string `toml:"proxies"`
	MaxBody int64    `toml:"max_body"`
	Model   struct {
		URL     string        `toml:"url"`
		Name    string        `toml:"name"`
		Context int           `toml:"context"`
		Threads int           `toml:"threads"`
		Think   string        `toml:"think"` // "": not sent; off or on
		Health  time.Duration `toml:"health"`
	} `toml:"model"`
	Line struct {
		Waiting   int           `toml:"waiting"`
		Deadline  time.Duration `toml:"deadline"`
		Heartbeat time.Duration `toml:"heartbeat"`
	} `toml:"line"`
	Log struct {
		Directory     string  `toml:"directory"`
		Level         string  `toml:"level"`
		MaxSizeMB     int64   `toml:"max_size_mb"`
		MaxTotalMB    int64   `toml:"max_total_size_mb"`
		RetentionDays float64 `toml:"retention_days"`
		Console       bool    `toml:"console"`
	} `toml:"log"`
	Apps struct {
		LWDescribe lwdescribe.Config `toml:"lwdescribe"`
	} `toml:"apps"`
}

func defaults() Config {
	var c Config
	c.Listen, c.Admin, c.Proxies, c.MaxBody = "127.0.0.1:8888", "127.0.0.1:8889", []string{"127.0.0.1"}, 8<<10
	c.Model.URL, c.Model.Name, c.Model.Context, c.Model.Threads = "http://127.0.0.1:11434", "qwen3:1.7b", 10240, 4
	c.Model.Think, c.Model.Health = "off", 15*time.Second
	c.Line.Waiting, c.Line.Deadline, c.Line.Heartbeat = 6, 150*time.Second, 10*time.Second
	c.Log.Directory, c.Log.Level, c.Log.MaxSizeMB, c.Log.MaxTotalMB, c.Log.RetentionDays = "/var/log/llmgate", "info", 10, 500, 14
	c.Apps.LWDescribe = lwdescribe.Defaults()
	return c
}

func main() {
	path := flag.String("c", "/usr/local/etc/llmgate/llmgate.toml", "configuration file")
	check := flag.Bool("check", false, "load the configuration and the apps, print them and exit")
	flag.Parse()
	if err := run(*path, *check); err != nil {
		fmt.Fprintln(os.Stderr, "llmgate:", err)
		os.Exit(1)
	}
}

func run(path string, check bool) error {
	cfg, err := load(path)
	if err != nil {
		return err
	}
	var proxies []netip.Addr
	for _, p := range cfg.Proxies {
		a, err := netip.ParseAddr(p)
		if err != nil {
			return fmt.Errorf("proxies: %w", err)
		}
		proxies = append(proxies, a.Unmap())
	}
	var apps []gate.App
	var summaries []string
	if cfg.Apps.LWDescribe.Enabled {
		a, err := lwdescribe.New(cfg.Apps.LWDescribe)
		if err != nil {
			return fmt.Errorf("lwdescribe: %w", err)
		}
		apps, summaries = append(apps, a), append(summaries, a.Summary())
	}
	for _, a := range apps {
		if err := a.Admission().Check(); err != nil {
			return fmt.Errorf("%s: %w", a.Name(), err)
		}
	}
	if check {
		fmt.Printf("configuration ok: %s, model %s at %s\n", path, cfg.Model.Name, cfg.Model.URL)
		for _, s := range summaries {
			fmt.Println(s)
		}
		return nil
	}
	ops, err := logger(cfg, "llmgate")
	if err != nil {
		return err
	}
	review, err := logger(cfg, "requests")
	if err != nil {
		return err
	}
	defer func() {
		review.Shutdown(5 * time.Second)
		ops.Shutdown(5 * time.Second)
	}()
	m := &model.Ollama{URL: cfg.Model.URL, Model: cfg.Model.Name, Context: cfg.Model.Context, Threads: cfg.Model.Threads,
		Client: &http.Client{}}
	if cfg.Model.Think != "" {
		on := cfg.Model.Think == "on"
		m.Think = &on
	}
	s := gate.New(gate.Config{Proxies: proxies, MaxBody: cfg.MaxBody, Deadline: cfg.Line.Deadline,
		Heartbeat: cfg.Line.Heartbeat, Waiting: cfg.Line.Waiting, Health: cfg.Model.Health}, m, apps, ops, review)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// requests share ctx: a signal ends waiting and running ones at once
	servers := []*http.Server{{Addr: cfg.Listen, Handler: s, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 75 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}}
	if cfg.Admin != "" {
		servers = append(servers, &http.Server{Addr: cfg.Admin, Handler: s.Status(), ReadHeaderTimeout: 5 * time.Second})
	}
	failed := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		}()
	}
	go s.Run(ctx)
	ops.Info("started", "listen", cfg.Listen, "admin", cfg.Admin, "model", cfg.Model.Name, "apps", strings.Join(summaries, "; "))
	select {
	case <-ctx.Done():
	case err = <-failed:
		ops.Error("listener failed", "error", err.Error())
	}
	stop()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.Shutdown(shut)
	}
	ops.Info("stopped")
	return err
}

// load is the defaults, then the file, then LLMGATE_ variables. A key or a
// variable that names no setting is refused: the library would drop it, and
// a misspelt limit would quietly be the default. So is a value start would
// refuse, so that -check passes only what starts.
func load(path string) (Config, error) {
	def := defaults()
	c, err := config.NewBuilder().WithTarget(&def).WithFile(path).WithEnvPrefix(envPrefix).WithArgs(nil).Build()
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	root, err := toml.NewParser(data).Parse()
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	known, vars := c.GetRegisteredPaths(), map[string]bool{}
	for p := range known {
		vars[envPrefix+strings.ToUpper(strings.ReplaceAll(p, ".", "_"))] = true // the library's own naming
	}
	var unknown []string
	for _, k := range paths(root, "") {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, envPrefix) && !vars[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return Config{}, fmt.Errorf("%s: unknown keys %s", path, strings.Join(unknown, ", "))
	}
	v, err := c.AsStruct()
	if err != nil {
		return Config{}, err
	}
	cfg := *v.(*Config)
	if _, ok := levels[cfg.Log.Level]; !ok {
		return Config{}, fmt.Errorf("log.level %q: one of debug, info, warn, error", cfg.Log.Level)
	}
	if !slices.Contains([]string{"", "off", "on"}, cfg.Model.Think) {
		return Config{}, fmt.Errorf("model.think %q: off, on or empty", cfg.Model.Think)
	}
	for name, d := range map[string]time.Duration{"model.health": cfg.Model.Health, "line.deadline": cfg.Line.Deadline, "line.heartbeat": cfg.Line.Heartbeat} {
		if d <= 0 {
			return Config{}, fmt.Errorf("%s %s: more than 0", name, d)
		}
	}
	return cfg, nil
}

const envPrefix = "LLMGATE_"

var levels = map[string]int64{"debug": log.LevelDebug, "info": log.LevelInfo, "warn": log.LevelWarn, "error": log.LevelError}

// paths are a table's keys, dotted, sorted
func paths(m map[string]any, prefix string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if sub, ok := m[k].(map[string]any); ok {
			out = append(out, paths(sub, prefix+k+".")...)
		} else {
			out = append(out, prefix+k)
		}
	}
	return out
}

// logger writes NAME.log in the log directory: JSON lines, rotated and aged
// out by the library
func logger(cfg Config, name string) (*log.Logger, error) {
	c := log.DefaultConfig()
	c.Name, c.Directory, c.Format, c.Sanitization = name, cfg.Log.Directory, "json", log.PolicyRaw
	c.EnableFile, c.EnableConsole = true, cfg.Log.Console
	c.MaxSizeKB, c.MaxTotalSizeKB, c.RetentionPeriodHrs = cfg.Log.MaxSizeMB*1000, cfg.Log.MaxTotalMB*1000, cfg.Log.RetentionDays*24
	c.Level = levels[cfg.Log.Level]
	l := log.NewLogger()
	if err := l.ApplyConfig(c); err != nil {
		return nil, fmt.Errorf("log %s: %w", name, err)
	}
	return l, l.Start()
}
