package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.json"), env(nil))
	if err == nil {
		t.Fatal("an explicit config file that does not exist must be an error")
	}
	c, err = Load("", env(map[string]string{"SUBSVC_CONFIG": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || c.DataDir != "/var/lib/subsvc" || c.Limits.MaxTestConcurrency != 20 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestEnvOverridesLegacyFile(t *testing.T) {
	p := writeFile(t, `{"listen":"0.0.0.0:1111","data_dir":"/srv/a","auth_token":"file-token-123","log_level":"debug","test":{"core_path":"/opt/xray","concurrency":50}}`)
	c, err := Load(p, env(map[string]string{"SUBSVC_LISTEN": "0.0.0.0:2222", "SUBSVC_AUTH_TOKEN": "env-token-123"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:2222" || c.AuthToken != "env-token-123" || c.TokenSource != "env" {
		t.Errorf("env must win: %+v", c)
	}
	if c.DataDir != "/srv/a" || c.LogLevel != "debug" || c.CorePath != "/opt/xray" {
		t.Errorf("legacy file values must be used where env is unset: %+v", c)
	}
}

func TestLegacyOperationalKeysAreIgnoredWithWarning(t *testing.T) {
	p := writeFile(t, `{"fetch_timeout_sec":5,"test":{"url":"https://x.example/","concurrency":99}}`)
	c, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "fetch_timeout_sec") || !strings.Contains(c.Warnings[0], "test") {
		t.Errorf("warnings: %v", c.Warnings)
	}
	if c.Limits.MaxTestConcurrency != 20 {
		t.Error("legacy test.concurrency must not leak into the hard limits")
	}
}

func TestInvalidEnv(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"listen":      {"SUBSVC_LISTEN": "8080"},
		"relative":    {"SUBSVC_DATA_DIR": "data"},
		"loglevel":    {"SUBSVC_LOG_LEVEL": "loud"},
		"cap zero":    {"SUBSVC_MAX_TEST_CONCURRENCY": "0"},
		"cap huge":    {"SUBSVC_MAX_TEST_CONCURRENCY": "500"},
		"bytes":       {"SUBSVC_MAX_SPEED_BYTES": "10"},
		"short token": {"SUBSVC_AUTH_TOKEN": "abc"},
		"bool":        {"SUBSVC_ALLOW_PRIVATE_TEST_URLS": "maybe"},
	} {
		m["SUBSVC_CONFIG"] = ""
		if _, err := Load("", env(m)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestEnsureToken(t *testing.T) {
	dir := t.TempDir()
	lo := &Config{Listen: "127.0.0.1:8080", DataDir: dir}
	if err := lo.EnsureToken(); err != nil || lo.AuthToken != "" {
		t.Fatalf("loopback without token must stay open: %v %q", err, lo.AuthToken)
	}

	c := &Config{Listen: "0.0.0.0:8080", DataDir: dir}
	if err := c.EnsureToken(); err != nil || len(c.AuthToken) != 48 || c.TokenSource != "generated" {
		t.Fatalf("generate: %v %+v", err, c)
	}
	st, err := os.Stat(filepath.Join(dir, "auth_token"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", err, st)
	}
	again := &Config{Listen: "0.0.0.0:8080", DataDir: dir}
	if err := again.EnsureToken(); err != nil || again.AuthToken != c.AuthToken || again.TokenSource != "stored" {
		t.Errorf("the stored token must be reused: %+v", again)
	}
	set := &Config{Listen: "0.0.0.0:8080", DataDir: dir, AuthToken: "configured-token"}
	if err := set.EnsureToken(); err != nil || set.AuthToken != "configured-token" {
		t.Error("a configured token must not be replaced")
	}
}

func TestSummaryHasNoSecret(t *testing.T) {
	c := &Config{Listen: "0.0.0.0:8080", AuthToken: "super-secret-token", TokenSource: "env"}
	for k, v := range c.Summary() {
		if s, ok := v.(string); ok && strings.Contains(s, "super-secret") {
			t.Errorf("secret leaked in %s", k)
		}
	}
}
