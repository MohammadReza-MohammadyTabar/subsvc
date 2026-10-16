// Package subscription downloads a subscription URL and turns its body
// (plain text or Base64) into a de-duplicated list of proxy nodes.
package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"subsvc/internal/proxy"
)

// Result is the parsed content of one subscription.
type Result struct {
	Nodes      []*proxy.Node
	Total      int // candidate lines (non-empty, non-comment)
	Invalid    int // lines that could not be parsed
	Duplicates int // repeated configs inside this subscription
	HTTPStatus int
	Bytes      int
}

// Fetcher downloads subscriptions.
type Fetcher struct {
	client    *http.Client
	maxBytes  int64
	userAgent string
	timeout   time.Duration
}

// NewFetcher builds a Fetcher. Keep-alives are disabled: fetches are rare and
// manual, so idle connections would only waste resources.
func NewFetcher(timeout time.Duration, maxBytes int64, userAgent string) *Fetcher {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	return &Fetcher{
		client:    &http.Client{Transport: tr, Timeout: timeout},
		maxBytes:  maxBytes,
		userAgent: userAgent,
		timeout:   timeout,
	}
}

// Fetch downloads and parses rawURL. A subscription that yields no valid
// config is reported as an error so that a broken source never replaces
// good, previously stored data.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, f.describe(ctx, err)
	}
	defer resp.Body.Close()

	res := &Result{HTTPStatus: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return res, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return res, f.describe(ctx, err)
	}
	if int64(len(body)) > f.maxBytes {
		return res, fmt.Errorf("response larger than %d bytes", f.maxBytes)
	}
	res.Bytes = len(body)

	parsed := Parse(body)
	parsed.HTTPStatus = resp.StatusCode
	parsed.Bytes = len(body)
	if len(parsed.Nodes) == 0 {
		if len(strings.TrimSpace(string(body))) == 0 {
			return parsed, errors.New("empty response")
		}
		return parsed, fmt.Errorf("no valid configs found (%d lines, %d invalid)", parsed.Total, parsed.Invalid)
	}
	return parsed, nil
}

// describe turns transport errors into short messages that never contain the
// URL (subscription URLs commonly embed secret tokens).
func (f *Fetcher) describe(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.New("cancelled")
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("timeout after %s", f.timeout)
	}
	return errors.New(err.Error())
}

// Parse decodes body (plain list or Base64) and parses every line.
func Parse(body []byte) *Result {
	text := Decode(body)
	res := &Result{}
	seen := make(map[string]struct{})
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		res.Total++
		n, err := proxy.Parse(line)
		if err != nil {
			res.Invalid++
			continue
		}
		if _, dup := seen[n.Key]; dup {
			res.Duplicates++
			continue
		}
		seen[n.Key] = struct{}{}
		res.Nodes = append(res.Nodes, n)
	}
	return res
}

// Decode returns the plain-text list of links contained in body. A body that
// already contains "://" is treated as plain text (Base64 never contains ':').
func Decode(body []byte) string {
	s := strings.TrimSpace(strings.TrimPrefix(string(body), "\xef\xbb\xbf"))
	if strings.Contains(s, "://") {
		return s
	}
	if dec, ok := decodeBase64(s); ok && strings.Contains(dec, "://") {
		return strings.TrimSpace(dec)
	}
	return s
}

func decodeBase64(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, "=")
	if s == "" {
		return "", false
	}
	for _, enc := range encodings {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}
