// Package config reads the DEPLOYMENT settings of the service: listen address,
// data directory, auth token, log level, core path and hard limits.
//
// Precedence, highest first:
//
//  1. environment variables with the SUBSVC_ prefix
//  2. the optional legacy config file (-config / SUBSVC_CONFIG); only the
//     deployment keys listen, data_dir, auth_token, log_level and
//     test.core_path are read from it (kept so existing installs keep working)
//  3. built-in defaults
//
// Operational settings (timeouts, test URLs, concurrency, speed test, success
// criteria, ranking, ...) are NOT read here. They live in internal/settings,
// are edited through the API/UI and stored in <data_dir>/settings.json.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"subsvc/internal/settings"
)

// LegacyPath is tried when no config file is given.
const LegacyPath = "/etc/subsvc/config.json"

// Config holds the deployment settings.
type Config struct {
	Listen    string
	DataDir   string
	AuthToken string
	LogLevel  string
	CorePath  string
	Limits    settings.Limits

	// TokenSource says where AuthToken came from: env, env-file, legacy-file,
	// generated, stored or none. Never contains the token itself.
	TokenSource string
	// Warnings are notes for the log (ignored legacy keys, ...).
	Warnings []string
}

// Defaults returns the built-in deployment defaults.
func Defaults() Config {
	return Config{
		Listen:   "127.0.0.1:8080",
		DataDir:  "/var/lib/subsvc",
		LogLevel: "info",
		CorePath: "/usr/local/bin/xray",
		Limits:   settings.DefaultLimits(),
	}
}

// Load applies defaults, then the legacy file, then the environment.
// legacyPath may be empty. getenv is os.Getenv in production.
func Load(legacyPath string, getenv func(string) string) (*Config, error) {
	c := Defaults()

	explicit := legacyPath != ""
	if !explicit {
		legacyPath = getenv("SUBSVC_CONFIG")
		explicit = legacyPath != ""
	}
	if !explicit {
		legacyPath = LegacyPath
	}
	if err := c.applyLegacyFile(legacyPath, explicit); err != nil {
		return nil, err
	}
	if err := c.applyEnv(getenv); err != nil {
		return nil, err
	}
	return &c, c.validate()
}

// legacyOperational lists keys of the old config.json that are now managed in
// the UI/API; they are ignored (with a warning) rather than treated as errors.
var legacyOperational = map[string]bool{
	"fetch_timeout_sec": true, "fetch_concurrency": true, "max_body_bytes": true, "user_agent": true, "test": true,
}

func (c *Config) applyLegacyFile(path string, mustExist bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !mustExist && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)) {
			return nil // optional file: absent (or unreadable by this user) is fine
		}
		return fmt.Errorf("read config file: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	str := func(key string, dst *string) error {
		v, ok := raw[key]
		if !ok {
			return nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return fmt.Errorf("config file %s: %s must be a string", path, key)
		}
		if s != "" {
			*dst = s
		}
		return nil
	}
	for key, dst := range map[string]*string{"listen": &c.Listen, "data_dir": &c.DataDir, "log_level": &c.LogLevel} {
		if err := str(key, dst); err != nil {
			return err
		}
	}
	if err := str("auth_token", &c.AuthToken); err != nil {
		return err
	}
	if c.AuthToken != "" {
		c.TokenSource = "legacy-file"
	}
	if t, ok := raw["test"]; ok {
		var test struct {
			CorePath string `json:"core_path"`
		}
		if err := json.Unmarshal(t, &test); err == nil && test.CorePath != "" {
			c.CorePath = test.CorePath
		}
	}
	var ignored []string
	for k := range raw {
		if legacyOperational[k] {
			ignored = append(ignored, k)
		}
	}
	if len(ignored) > 0 {
		sort.Strings(ignored)
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"config file %s: operational keys [%s] are ignored; they are managed in the UI/API (settings.json)",
			path, strings.Join(ignored, ", ")))
	}
	return nil
}

func (c *Config) applyEnv(getenv func(string) string) error {
	set := func(name string, dst *string) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			*dst = v
		}
	}
	set("SUBSVC_LISTEN", &c.Listen)
	set("SUBSVC_DATA_DIR", &c.DataDir)
	set("SUBSVC_LOG_LEVEL", &c.LogLevel)
	set("SUBSVC_CORE_PATH", &c.CorePath)

	if v := strings.TrimSpace(getenv("SUBSVC_AUTH_TOKEN")); v != "" {
		c.AuthToken, c.TokenSource = v, "env"
	} else if f := strings.TrimSpace(getenv("SUBSVC_AUTH_TOKEN_FILE")); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("SUBSVC_AUTH_TOKEN_FILE: %w", err)
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			c.AuthToken, c.TokenSource = t, "env-file"
		}
	}

	if v := strings.TrimSpace(getenv("SUBSVC_MAX_TEST_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return errors.New("SUBSVC_MAX_TEST_CONCURRENCY must be an integer between 1 and 64")
		}
		c.Limits.MaxTestConcurrency = n
	}
	if v := strings.TrimSpace(getenv("SUBSVC_MAX_SPEED_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 64<<10 || n > 64<<20 {
			return errors.New("SUBSVC_MAX_SPEED_BYTES must be an integer between 65536 and 67108864")
		}
		c.Limits.MaxSpeedBytes = n
	}
	if v := strings.TrimSpace(getenv("SUBSVC_ALLOW_PRIVATE_TEST_URLS")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("SUBSVC_ALLOW_PRIVATE_TEST_URLS must be true or false")
		}
		c.Limits.AllowPrivateURLs = b
	}
	return nil
}

func (c *Config) validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("SUBSVC_LISTEN %q must look like host:port (for example 0.0.0.0:8080): %w", c.Listen, err)
	}
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("SUBSVC_DATA_DIR %q must be an absolute path", c.DataDir)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("SUBSVC_LOG_LEVEL %q must be debug, info, warn or error", c.LogLevel)
	}
	if c.CorePath == "" {
		return errors.New("SUBSVC_CORE_PATH must not be empty")
	}
	if c.AuthToken != "" && len(c.AuthToken) < 8 {
		return errors.New("the auth token must be at least 8 characters")
	}
	return nil
}

// IsLoopback reports whether Listen only accepts local connections.
func (c *Config) IsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// EnsureToken makes sure a network-reachable API is never open: when no token
// is configured and the service does not listen on loopback only, a random
// token is generated once and stored (0600) in <data_dir>/auth_token, and
// reused on the next start. Its value is never logged; the file is the source.
// Listening on loopback without a token is allowed (local use).
func (c *Config) EnsureToken() error {
	if c.AuthToken != "" || c.IsLoopback() {
		return nil
	}
	path := filepath.Join(c.DataDir, "auth_token")
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 8 {
			c.AuthToken, c.TokenSource = t, "stored"
			return nil
		}
	}
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return fmt.Errorf("store generated auth token: %w", err)
	}
	c.AuthToken, c.TokenSource = tok, "generated"
	return nil
}

// Summary returns the non-secret deployment values, for the read-only part of
// the settings API/UI.
func (c *Config) Summary() map[string]any {
	return map[string]any{
		"listen":                 c.Listen,
		"data_dir":               c.DataDir,
		"log_level":              c.LogLevel,
		"core_path":              c.CorePath,
		"auth":                   c.AuthToken != "",
		"auth_token_source":      c.TokenSource,
		"max_test_concurrency":   c.Limits.MaxTestConcurrency,
		"max_speed_bytes":        c.Limits.MaxSpeedBytes,
		"allow_private_test_url": c.Limits.AllowPrivateURLs,
	}
}
