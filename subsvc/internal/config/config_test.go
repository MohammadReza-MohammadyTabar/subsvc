package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsAreValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Test.URL != "https://www.google.com/generate_204" || c.Test.TimeoutMS != 5000 || c.Test.Concurrency != 2 {
		t.Errorf("unexpected defaults: %+v", c.Test)
	}
}

func TestLoadOverridesAndKeepsDefaults(t *testing.T) {
	c, err := Load(write(t, `{"listen":"0.0.0.0:9000","test":{"timeout_ms":3000}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || c.Test.TimeoutMS != 3000 {
		t.Errorf("override not applied: %+v", c)
	}
	if c.Test.URL == "" || c.Test.CorePath == "" || c.DataDir == "" {
		t.Errorf("defaults lost: %+v", c)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	for name, content := range map[string]string{
		"typo":            `{"listn":"x"}`,
		"concurrency 3":   `{"test":{"concurrency":3}}`,
		"concurrency 0":   `{"test":{"concurrency":0}}`,
		"bad test url":    `{"test":{"url":"not a url"}}`,
		"tiny timeout":    `{"test":{"timeout_ms":5}}`,
		"bad log level":   `{"log_level":"loud"}`,
		"invalid json":    `{`,
		"empty listen":    `{"listen":""}`,
		"fetch timeout 0": `{"fetch_timeout_sec":0}`,
	} {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestExplicitMissingFileIsAnError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Errorf("got %v", err)
	}
}
