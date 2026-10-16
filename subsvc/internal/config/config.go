// Package config loads and validates the service configuration file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"time"
)

// DefaultPath is used when no -config flag is given.
const DefaultPath = "/etc/subsvc/config.json"

// MaxTestConcurrency is a hard ceiling to keep CPU/RAM low on small servers.
const MaxTestConcurrency = 200

// Test holds the settings of the real connectivity test.
type Test struct {
	URL              string `json:"url"`
	TimeoutMS        int    `json:"timeout_ms"`         // per-config probe timeout
	StartupTimeoutMS int    `json:"startup_timeout_ms"` // time allowed for the core to start listening
	Concurrency      int    `json:"concurrency"`        // 1..2
	CorePath         string `json:"core_path"`          // path of the xray binary
}

// Config is the whole configuration file.
type Config struct {
	Listen           string `json:"listen"`
	DataDir          string `json:"data_dir"`
	AuthToken        string `json:"auth_token"` // empty = no authentication
	LogLevel         string `json:"log_level"`  // debug|info|warn|error
	FetchTimeoutSec  int    `json:"fetch_timeout_sec"`
	FetchConcurrency int    `json:"fetch_concurrency"`
	MaxBodyBytes     int64  `json:"max_body_bytes"`
	UserAgent        string `json:"user_agent"`
	Test             Test   `json:"test"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Listen:           "127.0.0.1:8080",
		DataDir:          "/var/lib/subsvc",
		LogLevel:         "info",
		FetchTimeoutSec:  20,
		FetchConcurrency: 2,
		MaxBodyBytes:     10 << 20,
		UserAgent:        "subsvc/1.0",
		Test: Test{
			URL:              "https://www.google.com/generate_204",
			TimeoutMS:        5000,
			StartupTimeoutMS: 4000,
			Concurrency:      2,
			CorePath:         "/usr/local/bin/xray",
		},
	}
}

// Load reads path. If path is empty it tries DefaultPath and silently falls back
// to the defaults when that file does not exist. An explicit path must exist.
func Load(path string) (*Config, error) {
	cfg := Default()
	explicit := path != ""
	if !explicit {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return &cfg, cfg.Validate()
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // catch typos in key names
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks the values.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen must not be empty")
	}
	if c.DataDir == "" {
		return errors.New("data_dir must not be empty")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q must be debug, info, warn or error", c.LogLevel)
	}
	if c.FetchTimeoutSec < 1 || c.FetchTimeoutSec > 300 {
		return errors.New("fetch_timeout_sec must be between 1 and 300")
	}
	if c.FetchConcurrency < 1 || c.FetchConcurrency > 4 {
		return errors.New("fetch_concurrency must be between 1 and 4")
	}
	if c.MaxBodyBytes < 1024 {
		return errors.New("max_body_bytes must be at least 1024")
	}
	u, err := url.Parse(c.Test.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("test.url %q is not a valid http(s) URL", c.Test.URL)
	}
	if c.Test.TimeoutMS < 100 || c.Test.TimeoutMS > 120000 {
		return errors.New("test.timeout_ms must be between 100 and 120000")
	}
	if c.Test.StartupTimeoutMS < 500 || c.Test.StartupTimeoutMS > 60000 {
		return errors.New("test.startup_timeout_ms must be between 500 and 60000")
	}
	if c.Test.Concurrency < 1 || c.Test.Concurrency > MaxTestConcurrency {
		return fmt.Errorf("test.concurrency must be 1 or %d", MaxTestConcurrency)
	}
	if c.Test.CorePath == "" {
		return errors.New("test.core_path must not be empty")
	}
	return nil
}

// FetchTimeout returns the subscription download timeout.
func (c *Config) FetchTimeout() time.Duration { return time.Duration(c.FetchTimeoutSec) * time.Second }

// TestTimeout returns the per-config probe timeout.
func (c *Config) TestTimeout() time.Duration {
	return time.Duration(c.Test.TimeoutMS) * time.Millisecond
}

// StartupTimeout returns the time the core may take to start listening.
func (c *Config) StartupTimeout() time.Duration {
	return time.Duration(c.Test.StartupTimeoutMS) * time.Millisecond
}
