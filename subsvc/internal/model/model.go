// Package model contains the plain data types shared by all other packages.
package model

import "time"

// Link is a registered subscription URL.
type Link struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	URL       string       `json:"url"`
	Enabled   bool         `json:"enabled"`
	CreatedAt time.Time    `json:"created_at"`
	LastFetch *FetchStatus `json:"last_fetch,omitempty"`
}

// FetchStatus describes the outcome of the most recent fetch of a link.
type FetchStatus struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	HTTPStatus int       `json:"http_status,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Bytes      int       `json:"bytes,omitempty"`
	Total      int       `json:"total"`      // candidate lines
	Valid      int       `json:"valid"`      // unique, parseable configs
	Invalid    int       `json:"invalid"`    // lines that could not be parsed
	Duplicates int       `json:"duplicates"` // duplicates inside this link
	New        int       `json:"new"`        // configs not seen in any link before
}

// Error kinds reported in Probe.ErrorKind / Speed.ErrorKind.
const (
	ErrKindTimeout   = "timeout"     // no answer within the time limit
	ErrKindDNS       = "dns"         // a host name could not be resolved
	ErrKindTLS       = "tls"         // TLS handshake or certificate failure
	ErrKindConnect   = "connect"     // tunnel / TCP connection refused, reset or closed
	ErrKindHTTP      = "http_status" // a response arrived but it was not the expected status
	ErrKindCancelled = "cancelled"
	ErrKindOther     = "other"
)

// Probe is one HTTP request sent through the config's own tunnel
// (general internet check or YouTube check).
type Probe struct {
	Tested     bool   `json:"tested"`
	OK         bool   `json:"ok"`          // expected HTTP status (204) received through the tunnel
	LatencyMS  int64  `json:"latency_ms"`  // total time of the successful request (connect + response)
	ConnectMS  int64  `json:"connect_ms"`  // time until the tunnel (and TLS) to the target was established
	ResponseMS int64  `json:"response_ms"` // time from established tunnel until the HTTP response arrived
	StatusCode int    `json:"status_code"` // 0 if no response was received
	Error      string `json:"error,omitempty"`
	ErrorKind  string `json:"error_kind,omitempty"`
}

// Speed is the result of a real download through the tunnel.
type Speed struct {
	Tested     bool    `json:"tested"`
	OK         bool    `json:"ok"`          // the download finished without error
	Mbps       float64 `json:"mbps"`        // megabits per second, measured on the body transfer only
	Bytes      int64   `json:"bytes"`       // bytes actually downloaded
	DurationMS int64   `json:"duration_ms"` // whole speed test, request start to last byte
	TTFBMS     int64   `json:"ttfb_ms"`     // request start to first body byte
	Error      string  `json:"error,omitempty"`
	ErrorKind  string  `json:"error_kind,omitempty"`
	Skipped    string  `json:"skipped,omitempty"` // why the speed test was not run
}

// TestResult is the outcome of the real tests of one config. General, YouTube
// and Speed are evaluated independently. The flat fields below mirror General
// and are kept so that older API clients keep working.
type TestResult struct {
	Tested     bool       `json:"tested"`      // a test was actually attempted
	Reachable  bool       `json:"reachable"`   // legacy: General.OK
	LatencyMS  int64      `json:"latency_ms"`  // legacy: General.LatencyMS
	ConnectMS  int64      `json:"connect_ms"`  // legacy: General.ConnectMS
	ResponseMS int64      `json:"response_ms"` // legacy: General.ResponseMS
	StatusCode int        `json:"status_code"` // legacy: General.StatusCode
	Error      string     `json:"error,omitempty"`
	ErrorKind  string     `json:"error_kind,omitempty"`
	TestedAt   *time.Time `json:"tested_at,omitempty"`

	General *Probe `json:"general,omitempty"`
	YouTube *Probe `json:"youtube,omitempty"`
	Speed   *Speed `json:"speed,omitempty"`
}

// SyncLegacy copies General into the flat legacy fields. When only the core
// failed to start there is no General probe and Error is left as set.
func (t *TestResult) SyncLegacy() {
	if t.General == nil {
		return
	}
	g := t.General
	t.Reachable, t.LatencyMS, t.ConnectMS, t.ResponseMS = g.OK, g.LatencyMS, g.ConnectMS, g.ResponseMS
	t.StatusCode, t.Error, t.ErrorKind = g.StatusCode, g.Error, g.ErrorKind
}

// Normalize upgrades a result stored by an older version (flat fields only)
// so that General is always present for tested configs.
func (t *TestResult) Normalize() {
	if t.General != nil || !t.Tested {
		return
	}
	t.General = &Probe{Tested: true, OK: t.Reachable, LatencyMS: t.LatencyMS, ConnectMS: t.ConnectMS,
		ResponseMS: t.ResponseMS, StatusCode: t.StatusCode, Error: t.Error, ErrorKind: t.ErrorKind}
}

// Entry is one unique config (identified by its normalized URI).
type Entry struct {
	Key         string     `json:"key"`
	URI         string     `json:"uri"`
	Name        string     `json:"name"`
	Protocol    string     `json:"protocol"`
	Host        string     `json:"host,omitempty"`
	Port        int        `json:"port,omitempty"`
	Unsupported string     `json:"unsupported,omitempty"` // non-empty: cannot be tested by the core
	Sources     []string   `json:"sources"`               // link IDs that contain this config
	FirstSeen   time.Time  `json:"first_seen"`
	Test        TestResult `json:"test"`
}

// Stats summarises the store.
type Stats struct {
	Links       int `json:"links"`
	Configs     int `json:"configs"`
	Unsupported int `json:"unsupported"`
	Tested      int `json:"tested"`
	Reachable   int `json:"reachable"`    // general probe succeeded
	YouTubeOK   int `json:"youtube_ok"`   // YouTube probe succeeded
	SpeedTested int `json:"speed_tested"` // speed test finished
	Eligible    int `json:"eligible"`     // passes the success criteria (what the output contains)
}
