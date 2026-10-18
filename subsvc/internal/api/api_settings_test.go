package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"subsvc/internal/settings"
)

func (e *env) settingsGet() settingsResponse {
	e.t.Helper()
	code, body, _ := e.do("GET", "/api/settings", nil, "")
	if code != 200 {
		e.t.Fatalf("GET settings: %d %s", code, body)
	}
	var r settingsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) settingsPut(patch map[string]any) (int, []byte) {
	e.t.Helper()
	code, body, _ := e.do("PUT", "/api/settings", patch, "")
	return code, body
}

func (e *env) mustPut(patch map[string]any) {
	e.t.Helper()
	if code, body := e.settingsPut(patch); code != 200 {
		e.t.Fatalf("PUT %v: %d %s", patch, code, body)
	}
}

func TestSettingsDefaultsMatchSpecification(t *testing.T) {
	e := newEnv(t, "")
	s := e.settingsGet().Settings
	if s.TestConcurrency != 10 || s.TimeoutMS != 5000 || s.SpeedMaxBytes != 1<<20 ||
		s.GeneralURL != "https://www.google.com/generate_204" || s.YouTubeURL != "https://www.youtube.com/generate_204" {
		t.Errorf("defaults: %+v", s)
	}
}

func TestSettingsValidationAndPersistence(t *testing.T) {
	e := newEnv(t, "")
	orig := e.settingsGet().Settings

	for name, patch := range map[string]map[string]any{
		"concurrency too high":   {"test_concurrency": 200},
		"concurrency zero":       {"test_concurrency": 0},
		"timeout too small":      {"timeout_ms": 10},
		"speed bytes too big":    {"speed_max_bytes": 1 << 30},
		"loopback url":           {"general_url": "http://127.0.0.1/generate_204"},
		"localhost url":          {"youtube_url": "https://localhost/x"},
		"private ip url":         {"speed_url": "http://192.168.1.1/{bytes}"},
		"credentials in url":     {"general_url": "https://u:p@example.com/x"},
		"bad scheme":             {"general_url": "ftp://example.com/x"},
		"space in url":           {"general_url": "https://exa mple.com/"},
		"unknown field":          {"nope": 1},
		"bad rank mode":          {"rank_mode": "fastest"},
		"youtube required, off":  {"youtube_enabled": false},
		"nothing required":       {"require_general": false, "require_youtube": false},
		"min speed, speed off":   {"speed_enabled": false, "min_speed_mbps": 5},
		"negative min speed":     {"min_speed_mbps": -1},
		"both weights zero":      {"rank_weight_latency": 0, "rank_weight_speed": 0},
		"wrong type":             {"test_concurrency": "ten"},
		"header injection in UA": {"user_agent": "x\r\nHost: evil"},
	} {
		// private URLs are only accepted by this test env's limits (AllowPrivateURLs),
		// so check those two against a strict manager instead.
		if strings.Contains(name, "url") && (name == "loopback url" || name == "localhost url" || name == "private ip url") {
			continue
		}
		code, body := e.settingsPut(patch)
		if code != 400 {
			t.Errorf("%s: want 400, got %d %s", name, code, body)
			continue
		}
		var resp struct {
			Error  string            `json:"error"`
			Fields map[string]string `json:"fields"`
		}
		if json.Unmarshal(body, &resp) != nil || resp.Error == "" || len(resp.Fields) == 0 {
			t.Errorf("%s: error body must name the invalid fields: %s", name, body)
		}
	}
	if got := e.settingsGet().Settings; got != orig {
		t.Fatalf("a rejected update must not change anything:\n%+v\n%+v", got, orig)
	}

	e.mustPut(map[string]any{"test_concurrency": 3, "timeout_ms": 7000, "speed_max_bytes": 524288, "rank_mode": "speed"})
	want := e.settingsGet().Settings
	if want.TestConcurrency != 3 || want.TimeoutMS != 7000 || want.SpeedMaxBytes != 524288 || want.RankMode != "speed" {
		t.Fatalf("update not applied: %+v", want)
	}
	if want.GeneralURL != orig.GeneralURL {
		t.Error("fields that are not in the patch must keep their value")
	}

	// "restart": a new manager over the same directory sees the stored values.
	e.srv.Close()
	e.open("")
	if got := e.settingsGet().Settings; got != want {
		t.Errorf("settings lost after restart:\n%+v\n%+v", got, want)
	}

	code, body, _ := e.do("POST", "/api/settings/reset", nil, "")
	if code != 200 {
		t.Fatalf("reset: %d %s", code, body)
	}
	if got := e.settingsGet().Settings; got != settings.Default() {
		t.Errorf("reset must restore the defaults: %+v", got)
	}
}

func TestSettingsStrictLimitsRejectPrivateURLs(t *testing.T) {
	lim := settings.DefaultLimits() // AllowPrivateURLs=false, as in production
	for _, u := range []string{"http://127.0.0.1/x", "https://localhost/x", "http://192.168.1.1/x",
		"http://10.0.0.1/x", "http://169.254.169.254/latest", "http://[::1]/x", "http://printer.local/x"} {
		s := settings.Default()
		s.GeneralURL = u
		if err := s.Validate(lim); err == nil {
			t.Errorf("%s must be rejected", u)
		}
	}
	s := settings.Default()
	s.SpeedMaxBytes = lim.MaxSpeedBytes + 1
	if s.Validate(lim) == nil {
		t.Error("speed_max_bytes above the deployment limit must be rejected")
	}
	lim.MaxTestConcurrency = 4
	s = settings.Default() // test_concurrency 10 > limit 4
	if s.Validate(lim) == nil {
		t.Error("test_concurrency above the deployment limit must be rejected")
	}
}

// Criteria and ranking are applied when results are read, so changing them in
// the UI changes the output at once, without re-testing anything.
func TestCriteriaAndRankingApplyWithoutRetest(t *testing.T) {
	e := newEnv(t, "")
	e.probe.speeds["goodfast.example.com"] = 1  // low latency, slow download
	e.probe.speeds["goodslow.example.com"] = 50 // higher latency, fast download
	e.addLink("C", "/c")
	e.do("POST", "/api/refresh", nil, "")
	if job := e.waitDone(); job.State != "done" || job.TestTotal != 3 {
		t.Fatalf("job: %+v", job)
	}
	calls := e.probe.calls.Load()
	order := func() string { return strings.Join(hostsOf(t, e.output("")), ",") }
	const slow, fast, noyt = "goodslow.example.com", "goodfast.example.com", "noyt1.example.com"

	// default: YouTube required, ranked by score -> the config with real speed wins over lower latency
	if got := order(); got != slow+","+fast {
		t.Errorf("default (score) order = %s", got)
	}
	e.mustPut(map[string]any{"rank_mode": "latency"})
	if got := order(); got != fast+","+slow {
		t.Errorf("latency order = %s", got)
	}
	e.mustPut(map[string]any{"rank_mode": "speed"})
	if got := order(); got != slow+","+fast {
		t.Errorf("speed order = %s", got)
	}
	e.mustPut(map[string]any{"rank_mode": "latency", "require_youtube": false})
	if got := order(); got != fast+","+noyt+","+slow {
		t.Errorf("YouTube optional: order = %s", got)
	}
	e.mustPut(map[string]any{"require_youtube": true, "min_speed_mbps": 5})
	if got := order(); got != slow {
		t.Errorf("min speed 5 Mbps: %s", got)
	}
	e.mustPut(map[string]any{"min_speed_mbps": 0, "max_latency_ms": 300})
	if got := order(); got != fast {
		t.Errorf("max latency 300ms: %s", got)
	}
	if e.probe.calls.Load() != calls {
		t.Error("changing criteria or ranking must not trigger new tests")
	}

	// the configs API exposes the derived values and the three results separately
	_, body, _ := e.do("GET", "/api/configs", nil, "")
	var list []struct {
		Host     string  `json:"host"`
		Eligible bool    `json:"eligible"`
		Score    float64 `json:"score"`
		Test     struct {
			General *struct{ OK bool } `json:"general"`
			YouTube *struct{ OK bool } `json:"youtube"`
			Speed   *struct {
				Mbps float64 `json:"mbps"`
			} `json:"speed"`
		} `json:"test"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list) != 3 {
		t.Fatalf("configs: %v %s", err, body)
	}
	for _, c := range list {
		if c.Test.General == nil || c.Test.YouTube == nil || c.Test.Speed == nil {
			t.Errorf("%s: general/youtube/speed must be present: %+v", c.Host, c.Test)
		}
		if c.Host == noyt && (c.Test.YouTube.OK || !c.Test.General.OK || c.Eligible) {
			t.Errorf("noyt: %+v", c)
		}
		if c.Host == fast && (!c.Eligible || c.Score <= 0) {
			t.Errorf("fast: %+v", c)
		}
	}
	// legacy flat fields still there for older clients
	if !strings.Contains(string(body), `"reachable": true`) {
		t.Error("legacy 'reachable' field missing")
	}
}

func TestSettingsReachTheTesterAndApplyToNextJobOnly(t *testing.T) {
	e := newEnv(t, "")
	e.addLink("A", "/a")

	// job 1 runs with the default concurrency (10): all 3 testable configs run at once
	gate := make(chan struct{})
	e.probe.setGate(gate)
	e.do("POST", "/api/refresh", nil, "")
	waitFor(t, func() bool { return e.probe.cur.Load() == 3 }, "3 parallel tests with the default concurrency of 10")
	// the setting changes while the job runs: the job keeps its plan
	e.mustPut(map[string]any{"test_concurrency": 1, "speed_enabled": false, "min_speed_mbps": 0, "youtube_url": "https://www.youtube.com/generate_204"})
	_, body, _ := e.do("GET", "/api/status", nil, "")
	var st struct {
		Job struct {
			Plan struct {
				Concurrency int  `json:"concurrency"`
				Speed       bool `json:"speed"`
			} `json:"plan"`
		} `json:"job"`
	}
	json.Unmarshal(body, &st)
	if st.Job.Plan.Concurrency != 10 || !st.Job.Plan.Speed {
		t.Errorf("a running job must keep its settings: %+v", st.Job.Plan)
	}
	close(gate)
	e.waitDone()
	if !e.probe.lastSettings().SpeedEnabled {
		t.Error("job 1 should have used speed_enabled=true")
	}

	// job 2 uses the new settings: one test at a time, no speed test
	e.probe.setGate(nil)
	e.probe.max.Store(0)
	e.do("POST", "/api/test", nil, "")
	e.waitDone()
	if m := e.probe.max.Load(); m != 1 {
		t.Errorf("test_concurrency=1 but %d tests ran at once", m)
	}
	if e.probe.lastSettings().SpeedEnabled {
		t.Error("speed_enabled=false was not passed to the tester")
	}
	for _, c := range e.st.Entries("") {
		if c.Test.Speed != nil {
			t.Errorf("%s: speed result present although the speed test is disabled", c.Host)
		}
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestSettingsEndpointsRequireTheToken(t *testing.T) {
	e := newEnv(t, "s3cret-token")
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/settings"}, {"PUT", "/api/settings"}, {"POST", "/api/settings/reset"},
	} {
		for _, tok := range []string{"", "wrong"} {
			if code, _, _ := e.do(c.method, c.path, map[string]any{"timeout_ms": 6000}, tok); code != 401 {
				t.Errorf("%s %s token %q: %d", c.method, c.path, tok, code)
			}
		}
	}
	if got := e.cfg.Get().TimeoutMS; got != 5000 {
		t.Errorf("an unauthorised PUT changed the settings (timeout_ms=%d)", got)
	}
	code, body, _ := e.do("PUT", "/api/settings", map[string]any{"timeout_ms": 6000}, "s3cret-token")
	if code != 200 || strings.Contains(string(body), "s3cret-token") {
		t.Errorf("authorised PUT: %d (the response must not contain the token) %s", code, body)
	}
}
