// Package settings holds the operational settings that can be changed at
// runtime (through the API / UI). They are validated, stored in
// <data_dir>/settings.json and survive restarts.
//
// Precedence (see README):
//   - deployment settings (listen address, data dir, token, core path, hard
//     limits) come only from SUBSVC_* environment variables and are NOT part
//     of this package;
//   - operational settings come from settings.json, and from the built-in
//     defaults for every field not stored yet. Environment variables never
//     override them, except that the hard limits (Limits) bound their range.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"subsvc/internal/model"
)

// Rank modes.
const (
	RankScore   = "score"
	RankLatency = "latency"
	RankSpeed   = "speed"
)

// SpeedPlaceholder in speed_url is replaced with speed_max_bytes.
const SpeedPlaceholder = "{bytes}"

// Limits are deployment-level hard limits (from environment variables).
type Limits struct {
	MaxTestConcurrency int   // upper bound of test_concurrency / speed_concurrency
	MaxSpeedBytes      int64 // upper bound of speed_max_bytes
	AllowPrivateURLs   bool  // allow loopback/private hosts in test URLs (local testing only)
}

// DefaultLimits are used when no environment variable is set.
func DefaultLimits() Limits {
	return Limits{MaxTestConcurrency: 20, MaxSpeedBytes: 8 << 20}
}

// Settings is the complete set of runtime-editable settings.
type Settings struct {
	// --- test execution
	TestConcurrency  int `json:"test_concurrency"`   // configs tested in parallel (one Xray process each)
	TimeoutMS        int `json:"timeout_ms"`         // timeout of each connectivity probe
	StartupTimeoutMS int `json:"startup_timeout_ms"` // time the core may take to start

	// --- connectivity probes
	GeneralURL     string `json:"general_url"`
	YouTubeEnabled bool   `json:"youtube_enabled"`
	YouTubeURL     string `json:"youtube_url"`

	// --- speed test
	SpeedEnabled     bool   `json:"speed_enabled"`
	SpeedURL         string `json:"speed_url"` // may contain {bytes}
	SpeedMaxBytes    int64  `json:"speed_max_bytes"`
	SpeedTimeoutMS   int    `json:"speed_timeout_ms"`
	SpeedConcurrency int    `json:"speed_concurrency"` // downloads running at the same time

	// --- success criteria (what goes into the output)
	RequireGeneral bool    `json:"require_general"`
	RequireYouTube bool    `json:"require_youtube"`
	MaxLatencyMS   int     `json:"max_latency_ms"` // 0 = no limit; applies to every required probe
	MinSpeedMbps   float64 `json:"min_speed_mbps"` // 0 = no minimum; needs speed_enabled

	// --- ranking
	RankMode          string  `json:"rank_mode"`           // score | latency | speed
	RankWeightLatency int     `json:"rank_weight_latency"` // used by rank_mode=score
	RankWeightSpeed   int     `json:"rank_weight_speed"`
	SpeedRefMbps      float64 `json:"speed_ref_mbps"` // speed that earns the full speed score

	// --- subscription download
	FetchTimeoutSec   int    `json:"fetch_timeout_sec"`
	FetchConcurrency  int    `json:"fetch_concurrency"`
	FetchMaxBodyBytes int64  `json:"fetch_max_body_bytes"`
	UserAgent         string `json:"user_agent"`
}

// Default returns the built-in defaults.
func Default() Settings {
	return Settings{
		TestConcurrency:  10,
		TimeoutMS:        5000,
		StartupTimeoutMS: 5000,

		GeneralURL:     "https://www.google.com/generate_204",
		YouTubeEnabled: true,
		YouTubeURL:     "https://www.youtube.com/generate_204",

		SpeedEnabled:     true,
		SpeedURL:         "https://speed.cloudflare.com/__down?bytes=" + SpeedPlaceholder,
		SpeedMaxBytes:    1 << 20,
		SpeedTimeoutMS:   15000,
		SpeedConcurrency: 2,

		RequireGeneral: true,
		RequireYouTube: true,

		RankMode:          RankScore,
		RankWeightLatency: 40,
		RankWeightSpeed:   60,
		SpeedRefMbps:      10,

		FetchTimeoutSec:   20,
		FetchConcurrency:  2,
		FetchMaxBodyBytes: 10 << 20,
		UserAgent:         "subsvc/1.0",
	}
}

// ValidationError lists every invalid field.
type ValidationError struct {
	Fields map[string]string `json:"fields"`
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + e.Fields[k]
	}
	return "invalid settings: " + strings.Join(parts, "; ")
}

// Validate checks every field against the hard limits.
func (s Settings) Validate(lim Limits) error {
	bad := map[string]string{}
	rng := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			bad[name] = fmt.Sprintf("must be between %d and %d", lo, hi)
		}
	}
	rng("test_concurrency", s.TestConcurrency, 1, lim.MaxTestConcurrency)
	rng("speed_concurrency", s.SpeedConcurrency, 1, lim.MaxTestConcurrency)
	rng("timeout_ms", s.TimeoutMS, 500, 60000)
	rng("startup_timeout_ms", s.StartupTimeoutMS, 500, 60000)
	rng("speed_timeout_ms", s.SpeedTimeoutMS, 1000, 120000)
	rng("max_latency_ms", s.MaxLatencyMS, 0, 60000)
	rng("rank_weight_latency", s.RankWeightLatency, 0, 100)
	rng("rank_weight_speed", s.RankWeightSpeed, 0, 100)
	rng("fetch_timeout_sec", s.FetchTimeoutSec, 1, 300)
	rng("fetch_concurrency", s.FetchConcurrency, 1, 4)

	if s.SpeedMaxBytes < 64<<10 || s.SpeedMaxBytes > lim.MaxSpeedBytes {
		bad["speed_max_bytes"] = fmt.Sprintf("must be between %d and %d", 64<<10, lim.MaxSpeedBytes)
	}
	if s.FetchMaxBodyBytes < 1024 || s.FetchMaxBodyBytes > 64<<20 {
		bad["fetch_max_body_bytes"] = "must be between 1024 and 67108864"
	}
	if !(s.MinSpeedMbps >= 0 && s.MinSpeedMbps <= 10000) { // also rejects NaN
		bad["min_speed_mbps"] = "must be between 0 and 10000"
	}
	if !(s.SpeedRefMbps > 0 && s.SpeedRefMbps <= 10000) {
		bad["speed_ref_mbps"] = "must be greater than 0 and at most 10000"
	}
	switch s.RankMode {
	case RankScore, RankLatency, RankSpeed:
	default:
		bad["rank_mode"] = "must be score, latency or speed"
	}
	if s.RankMode == RankScore && s.RankWeightLatency+s.RankWeightSpeed == 0 {
		bad["rank_weight_speed"] = "weights must not both be 0 when rank_mode is score"
	}
	if ua := strings.TrimSpace(s.UserAgent); ua == "" || len(ua) > 200 || strings.ContainsAny(ua, "\r\n") {
		bad["user_agent"] = "must be 1-200 characters without line breaks"
	}

	if err := checkURL(s.GeneralURL, lim.AllowPrivateURLs); err != nil {
		bad["general_url"] = err.Error()
	}
	if err := checkURL(s.YouTubeURL, lim.AllowPrivateURLs); err != nil {
		bad["youtube_url"] = err.Error()
	}
	if err := checkURL(strings.ReplaceAll(s.SpeedURL, SpeedPlaceholder, "1048576"), lim.AllowPrivateURLs); err != nil {
		bad["speed_url"] = err.Error()
	}

	if !s.RequireGeneral && !s.RequireYouTube {
		bad["require_general"] = "at least one of require_general / require_youtube must be true"
	}
	if s.RequireYouTube && !s.YouTubeEnabled {
		bad["require_youtube"] = "requires youtube_enabled"
	}
	if s.MinSpeedMbps > 0 && !s.SpeedEnabled {
		bad["min_speed_mbps"] = "requires speed_enabled"
	}
	if len(bad) > 0 {
		return &ValidationError{Fields: bad}
	}
	return nil
}

// checkURL validates a test URL. Test traffic leaves through a remote proxy,
// but literal loopback/private targets are still rejected: they are never a
// meaningful internet test and would probe the proxy server's own network.
func checkURL(raw string, allowPrivate bool) error {
	if raw == "" || len(raw) > 2048 {
		return errors.New("must be 1-2048 characters")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return errors.New("must not contain spaces or control characters")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("missing host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("invalid port")
		}
	}
	if allowPrivate {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return errors.New("local host names are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("loopback, private and link-local addresses are not allowed")
		}
	}
	return nil
}

// ------------------------------------------------------------ derived values

// Timeout and friends convert the stored numbers.
func (s Settings) SpeedURLFor() string {
	return strings.ReplaceAll(s.SpeedURL, SpeedPlaceholder, strconv.FormatInt(s.SpeedMaxBytes, 10))
}

// ProbeLatency returns the latency used for limits and ranking: the general
// probe when it succeeded, otherwise the YouTube probe. 0 means "no data".
func ProbeLatency(t model.TestResult) int64 {
	if t.General != nil && t.General.OK {
		return t.General.LatencyMS
	}
	if t.YouTube != nil && t.YouTube.OK {
		return t.YouTube.LatencyMS
	}
	return 0
}

// ProbesPass reports whether the connectivity probes meet the criteria. It is
// also the gate that decides whether a speed test is worth running.
func (s Settings) ProbesPass(t model.TestResult) bool {
	if s.RequireGeneral && !(t.General != nil && t.General.OK) {
		return false
	}
	if s.RequireYouTube && s.YouTubeEnabled && !(t.YouTube != nil && t.YouTube.OK) {
		return false
	}
	if s.MaxLatencyMS > 0 {
		if s.RequireGeneral && t.General.LatencyMS > int64(s.MaxLatencyMS) {
			return false
		}
		if s.RequireYouTube && s.YouTubeEnabled && t.YouTube.LatencyMS > int64(s.MaxLatencyMS) {
			return false
		}
	}
	return true
}

// Eligible reports whether a config meets every success criterion, i.e.
// whether it belongs in the output.
func (s Settings) Eligible(t model.TestResult) bool {
	if !t.Tested || !s.ProbesPass(t) {
		return false
	}
	if s.MinSpeedMbps > 0 {
		if t.Speed == nil || !t.Speed.OK || t.Speed.Mbps < s.MinSpeedMbps {
			return false
		}
	}
	return true
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Score returns 0..100. Latency is judged against the probe timeout (0 ms =
// best, timeout = 0); speed against speed_ref_mbps. A config whose speed was
// not measured (speed test disabled) is scored on latency alone; if speed is
// enabled but the download failed it earns 0 for the speed part.
func (s Settings) Score(t model.TestResult) float64 {
	lat := 0.0
	if l := ProbeLatency(t); l > 0 {
		lat = clamp01(1 - float64(l)/float64(s.TimeoutMS))
	}
	if !s.SpeedEnabled {
		return round1(lat * 100)
	}
	sp := 0.0
	if t.Speed != nil && t.Speed.OK {
		sp = clamp01(t.Speed.Mbps / s.SpeedRefMbps)
	}
	wl, ws := float64(s.RankWeightLatency), float64(s.RankWeightSpeed)
	if wl+ws == 0 {
		return round1(lat * 100)
	}
	return round1((wl*lat + ws*sp) / (wl + ws) * 100)
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// EntryView is an entry plus values derived from the current settings. They
// are computed on every read, so changing the criteria takes effect at once
// without re-testing.
type EntryView struct {
	model.Entry
	Eligible bool    `json:"eligible"`
	Score    float64 `json:"score"`
}

// View derives the view of an entry.
func (s Settings) View(e model.Entry) EntryView {
	return EntryView{Entry: e, Eligible: s.Eligible(e.Test), Score: s.Score(e.Test)}
}

// Rank sorts views best-first according to rank_mode. Ties fall back to the
// lower latency, then to the key, so the order is stable.
func (s Settings) Rank(v []EntryView) {
	speed := func(e EntryView) float64 {
		if e.Test.Speed != nil && e.Test.Speed.OK {
			return e.Test.Speed.Mbps
		}
		return -1
	}
	lat := func(e EntryView) int64 {
		if l := ProbeLatency(e.Test); l > 0 {
			return l
		}
		return 1 << 40
	}
	sort.SliceStable(v, func(i, j int) bool {
		a, b := v[i], v[j]
		switch s.RankMode {
		case RankLatency:
		case RankSpeed:
			if speed(a) != speed(b) {
				return speed(a) > speed(b)
			}
		default:
			if a.Score != b.Score {
				return a.Score > b.Score
			}
		}
		if lat(a) != lat(b) {
			return lat(a) < lat(b)
		}
		return a.Key < b.Key
	})
}

// -------------------------------------------------------------------- Manager

// Manager owns the current settings and their persistence.
type Manager struct {
	path string
	lim  Limits

	mu  sync.RWMutex
	cur Settings
}

// Open loads <dir>/settings.json (missing = defaults). Fields missing from the
// file keep their defaults, so adding a setting in a new version is safe.
// Values above a lowered hard limit are clamped instead of refusing to start.
func Open(dir string, lim Limits) (*Manager, error) {
	m := &Manager{path: filepath.Join(dir, "settings.json"), lim: lim, cur: Default()}
	data, err := os.ReadFile(m.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", m.path, err)
	default:
		if err := decodeInto(data, &m.cur, false); err != nil {
			return nil, fmt.Errorf("%s is corrupt (fix or remove it): %w", m.path, err)
		}
	}
	if m.cur.TestConcurrency > lim.MaxTestConcurrency {
		m.cur.TestConcurrency = lim.MaxTestConcurrency
	}
	if m.cur.SpeedConcurrency > lim.MaxTestConcurrency {
		m.cur.SpeedConcurrency = lim.MaxTestConcurrency
	}
	if m.cur.SpeedMaxBytes > lim.MaxSpeedBytes {
		m.cur.SpeedMaxBytes = lim.MaxSpeedBytes
	}
	if err := m.cur.Validate(lim); err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	return m, nil
}

func decodeInto(data []byte, dst *Settings, strict bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// Limits returns the hard limits (for display).
func (m *Manager) Limits() Limits { return m.lim }

// Get returns a copy of the current settings. Take one snapshot per job so a
// job is never affected by a change made while it runs.
func (m *Manager) Get() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur
}

// Update merges the JSON patch (only the fields present) into the current
// settings, validates the result and stores it atomically. Nothing changes if
// validation or writing fails.
func (m *Manager) Update(patch []byte) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.cur
	if err := decodeInto(patch, &next, true); err != nil {
		return m.cur, &ValidationError{Fields: map[string]string{"body": err.Error()}}
	}
	return m.commit(next)
}

// Reset restores the defaults.
func (m *Manager) Reset() (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commit(Default())
}

func (m *Manager) commit(next Settings) (Settings, error) {
	next.UserAgent = strings.TrimSpace(next.UserAgent)
	next.GeneralURL = strings.TrimSpace(next.GeneralURL)
	next.YouTubeURL = strings.TrimSpace(next.YouTubeURL)
	next.SpeedURL = strings.TrimSpace(next.SpeedURL)
	if err := next.Validate(m.lim); err != nil {
		return m.cur, err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return m.cur, err
	}
	if err := writeAtomic(m.path, data); err != nil {
		return m.cur, fmt.Errorf("save settings: %w", err)
	}
	m.cur = next
	return next, nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(e error) error { tmp.Close(); os.Remove(name); return e }
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
