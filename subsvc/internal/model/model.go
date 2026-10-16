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

// TestResult is the outcome of a real connectivity test through a config's own proxy.
type TestResult struct {
	Tested     bool       `json:"tested"`      // a test was actually attempted
	Reachable  bool       `json:"reachable"`   // HTTP 204 received through the proxy
	LatencyMS  int64      `json:"latency_ms"`  // total time of the successful request (connect + response)
	ConnectMS  int64      `json:"connect_ms"`  // time until the tunnel (and TLS) to the target was established
	ResponseMS int64      `json:"response_ms"` // time from established tunnel until the HTTP response arrived
	StatusCode int        `json:"status_code"` // HTTP status, 0 if none was received
	Error      string     `json:"error,omitempty"`
	TestedAt   *time.Time `json:"tested_at,omitempty"`
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
	Reachable   int `json:"reachable"`
}
