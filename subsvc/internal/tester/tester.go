// Package tester performs the real tests of one config. It starts the proxy
// core (Xray) with that single config, exposes it only as a SOCKS5 listener on
// a random port on 127.0.0.1, and sends every request THROUGH that listener:
//
//   - a general connectivity probe  (default https://www.google.com/generate_204)
//   - a YouTube probe               (default https://www.youtube.com/generate_204)
//   - an optional, size-limited speed download
//
// The three are evaluated independently. The core only lives for the duration
// of one test, and a direct connection is impossible: the HTTP transport can
// only dial the local listener of that core (see guardedDial).
package tester

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"subsvc/internal/model"
	"subsvc/internal/proxy"
	"subsvc/internal/settings"
)

// Options are the deployment-level parameters of the Tester. Everything that
// users can change at runtime arrives with each Test call as settings.Settings.
type Options struct {
	CorePath string   // xray binary
	WorkDir  string   // directory for short-lived private files (created 0700)
	ExtraEnv []string // extra KEY=VALUE for the core process (used by tests)
}

// Tester runs real tests. It holds no state and is safe for concurrent use.
type Tester struct {
	opts Options
}

// New returns a Tester.
func New(o Options) *Tester { return &Tester{opts: o} }

// Gate limits how many speed downloads run at the same time. Create it per
// job with a buffer of settings.SpeedConcurrency. A nil Gate means no limit.
type Gate chan struct{}

// NewGate returns a Gate of the given size.
func NewGate(n int) Gate {
	if n < 1 {
		n = 1
	}
	return make(Gate, n)
}

func (g Gate) acquire(ctx context.Context) bool {
	if g == nil {
		return true
	}
	select {
	case g <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g Gate) release() {
	if g != nil {
		<-g
	}
}

// Test checks one node. It never panics and always returns a result.
func (t *Tester) Test(ctx context.Context, n *proxy.Node, s settings.Settings, gate Gate) model.TestResult {
	res := model.TestResult{}
	ob, err := n.Outbound()
	if err != nil {
		res.Error = err.Error() // Tested stays false: nothing was attempted
		return res
	}
	res.Tested = true
	now := time.Now().UTC()
	res.TestedAt = &now

	var (
		core     *core
		startErr error
	)
	for attempt := 0; attempt < 3; attempt++ {
		core, startErr = t.startCore(ctx, ob, time.Duration(s.StartupTimeoutMS)*time.Millisecond)
		// Only an early core exit can be a lost race; anything else is final.
		if startErr == nil || ctx.Err() != nil || !strings.HasPrefix(startErr.Error(), "core exited") {
			break
		}
	}
	if startErr != nil {
		if ctx.Err() != nil {
			res.Tested, res.TestedAt = false, nil
			return res
		}
		res.General = &model.Probe{Tested: true, Error: startErr.Error(), ErrorKind: model.ErrKindOther}
		res.SyncLegacy()
		return res
	}
	defer core.stop()

	timeout := time.Duration(s.TimeoutMS) * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the two probes run in parallel: a dead tunnel costs one timeout, not two
		defer wg.Done()
		res.General = t.probe(ctx, core, s.GeneralURL, timeout)
	}()
	if s.YouTubeEnabled {
		res.YouTube = t.probe(ctx, core, s.YouTubeURL, timeout)
	}
	wg.Wait()
	res.SyncLegacy()
	if ctx.Err() != nil {
		return res
	}

	if s.SpeedEnabled {
		if !s.ProbesPass(res) {
			res.Speed = &model.Speed{Skipped: "connectivity criteria not met"}
		} else if !gate.acquire(ctx) {
			res.Speed = &model.Speed{Skipped: "cancelled"}
		} else {
			res.Speed = t.speed(ctx, core, s)
			gate.release()
		}
	}
	return res
}

// ------------------------------------------------------------------- core

type core struct {
	addr string // 127.0.0.1:<port> of the SOCKS5 inbound
	tail *tailBuffer
	stop func()
}

func (t *Tester) coreEnv() []string {
	// Minimal environment: the core must never see SUBSVC_AUTH_TOKEN or other
	// secrets. One thread and a soft memory limit keep 10 parallel cores cheap.
	env := []string{"PATH=" + os.Getenv("PATH"), "GOMAXPROCS=1", "GOMEMLIMIT=96MiB"}
	return append(env, t.opts.ExtraEnv...)
}

// startCore launches the core with a SOCKS5 inbound on a free local port and
// waits until the port accepts connections. stop terminates the core and
// removes every file it created. (Xray 26.x does not create unix-socket
// SOCKS inbounds, so a loopback TCP port is used.)
func (t *Tester) startCore(ctx context.Context, ob map[string]any, startup time.Duration) (*core, error) {
	if err := os.MkdirAll(t.opts.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("no free local port: %w", err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	// The directory holds credentials: private (0700), removed as soon as the
	// core stops.
	dir, err := os.MkdirTemp(t.opts.WorkDir, "r-")
	if err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "info"}, // info shows why an outbound dial failed (DNS)
		"inbounds": []any{map[string]any{
			"tag": "in", "listen": "127.0.0.1", "port": port, "protocol": "socks",
			"settings": map[string]any{"auth": "noauth", "udp": false},
		}},
		"outbounds": []any{ob}, // the only outbound: there is no "direct" to fall back to
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("write core config: %w", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, t.opts.CorePath, "run", "-config", file)
	tail := &tailBuffer{max: 4096}
	cmd.Stdout, cmd.Stderr = tail, tail
	cmd.Env = t.coreEnv()
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("start core: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() {
		cancel() // CommandContext kills the process
		<-exited
		os.RemoveAll(dir)
	}

	deadline := time.NewTimer(startup)
	defer deadline.Stop()
	for {
		select {
		case <-exited:
			os.RemoveAll(dir)
			cancel()
			return nil, fmt.Errorf("core exited: %s", tail.summary())
		case <-ctx.Done():
			stop()
			return nil, errors.New("cancelled")
		case <-deadline.C:
			stop()
			return nil, fmt.Errorf("core did not start within %s: %s", startup, tail.summary())
		default:
		}
		if c, derr := net.DialTimeout("tcp", addr, 150*time.Millisecond); derr == nil {
			c.Close()
			select { // the listener belongs to someone else if the core already died
			case <-exited:
				os.RemoveAll(dir)
				cancel()
				return nil, fmt.Errorf("core exited: %s", tail.summary())
			default:
			}
			return &core{addr: addr, tail: tail, stop: stop}, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// guardedDial is the ONLY way the HTTP transport can open a connection: to the
// local SOCKS5 listener of this test's core. Any attempt to reach a real address
// directly (a transport bug, a stray environment proxy, a redirect) is refused,
// so a test can never silently go around the tunnel.
func guardedDial(coreAddr string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != socksPlaceholder {
			return nil, fmt.Errorf("blocked direct connection to %s", addr)
		}
		return d.DialContext(ctx, "tcp", coreAddr)
	}
}

// socksPlaceholder is the (never resolved) proxy address in the transport's
// proxy URL; guardedDial maps it to the core's local listener.
const socksPlaceholder = "tunnel.invalid:1080"

func (c *core) transport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyURL(&url.URL{Scheme: "socks5", Host: socksPlaceholder}),
		DialContext:           guardedDial(c.addr),
		DisableKeepAlives:     true,
		DisableCompression:    true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// -------------------------------------------------------------- probes

// probe sends GET target through the tunnel and requires HTTP 204.
func (t *Tester) probe(ctx context.Context, c *core, target string, timeout time.Duration) *model.Probe {
	p := &model.Probe{Tested: true}
	tr := c.transport(timeout)
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a redirect is not a 204
		},
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var gotConn time.Time
	trace := &httptrace.ClientTrace{
		// Fires once the tunnel to the target (SOCKS CONNECT through the proxy
		// outbound, plus TLS for https) is established.
		GotConn: func(httptrace.GotConnInfo) { gotConn = time.Now() },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(pctx, trace), http.MethodGet, target, nil)
	if err != nil {
		p.Error, p.ErrorKind = "invalid test URL", model.ErrKindOther
		return p
	}
	req.Header.Set("User-Agent", "subsvc-probe/1.0")
	req.Header.Set("Cache-Control", "no-cache")

	start := time.Now()
	resp, err := client.Do(req)
	end := time.Now()
	if !gotConn.IsZero() {
		p.ConnectMS = gotConn.Sub(start).Milliseconds()
	}
	if err != nil {
		p.ErrorKind, p.Error = classify(ctx, pctx, err, timeout, c.tail)
		return p
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	p.StatusCode = resp.StatusCode
	if resp.StatusCode != http.StatusNoContent {
		p.ErrorKind = model.ErrKindHTTP
		p.Error = fmt.Sprintf("unexpected HTTP status %d (expected 204)", resp.StatusCode)
		return p
	}
	p.OK = true
	p.LatencyMS = end.Sub(start).Milliseconds()
	if !gotConn.IsZero() {
		p.ResponseMS = end.Sub(gotConn).Milliseconds()
	}
	return p
}

// speed downloads up to SpeedMaxBytes through the tunnel and measures the
// transfer. The throughput is computed on the body transfer only (after the
// first bytes arrived), so connection setup time does not distort it.
func (t *Tester) speed(ctx context.Context, c *core, s settings.Settings) *model.Speed {
	sp := &model.Speed{Tested: true}
	timeout := time.Duration(s.SpeedTimeoutMS) * time.Millisecond
	tr := c.transport(timeout)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(sctx, http.MethodGet, s.SpeedURLFor(), nil)
	if err != nil {
		sp.Error, sp.ErrorKind = "invalid speed URL", model.ErrKindOther
		return sp
	}
	req.Header.Set("User-Agent", "subsvc-probe/1.0")
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", s.SpeedMaxBytes-1))

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		sp.DurationMS = time.Since(start).Milliseconds()
		sp.ErrorKind, sp.Error = classify(ctx, sctx, err, timeout, c.tail)
		return sp
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		sp.DurationMS = time.Since(start).Milliseconds()
		sp.ErrorKind = model.ErrKindHTTP
		sp.Error = fmt.Sprintf("unexpected HTTP status %d", resp.StatusCode)
		return sp
	}

	var (
		buf        = make([]byte, 32<<10)
		total      int64
		first      int64 // bytes of the first read (excluded from the rate)
		firstAt    time.Time
		readErr    error
		finishedOK bool
	)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if total == 0 {
				firstAt = time.Now()
				sp.TTFBMS = firstAt.Sub(start).Milliseconds()
				first = int64(n)
			}
			total += int64(n)
		}
		if total >= s.SpeedMaxBytes { // got everything we asked for; stop reading
			total = s.SpeedMaxBytes
			finishedOK = true
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				finishedOK = total > 0
				if total == 0 {
					readErr = errors.New("empty response body")
				}
			} else {
				readErr = err
			}
			break
		}
	}
	end := time.Now()
	sp.Bytes = total
	sp.DurationMS = end.Sub(start).Milliseconds()
	if total > first && !firstAt.IsZero() { // rate over the body transfer, excluding the first chunk
		if secs := end.Sub(firstAt).Seconds(); secs > 0 {
			sp.Mbps = round2(float64(total-first) * 8 / secs / 1e6)
		}
	} else if total > 0 { // a single chunk: fall back to the whole request
		if secs := end.Sub(start).Seconds(); secs > 0 {
			sp.Mbps = round2(float64(total) * 8 / secs / 1e6)
		}
	}
	if finishedOK {
		sp.OK = true
		return sp
	}
	if readErr == nil {
		readErr = io.ErrUnexpectedEOF
	}
	if readErr.Error() == "empty response body" {
		sp.ErrorKind, sp.Error = model.ErrKindOther, "empty response body"
		return sp
	}
	sp.ErrorKind, sp.Error = classify(ctx, sctx, readErr, timeout, c.tail)
	return sp
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// -------------------------------------------------------- error handling

var (
	localAddrRe = regexp.MustCompile(`tcp 127\.0\.0\.1:\d+->`)
	dnsLogRe    = regexp.MustCompile(`(?i)no such host|server misbehaving|lookup [^ ]+ on |temporary failure in name resolution`)
)

// classify turns a transport error into a stable kind and a short message.
//
// Note on DNS: the names of the TEST targets are resolved by the proxy server,
// not here, so a failure there is invisible to the client and looks like a
// closed tunnel. DNS is reported when the core itself failed to resolve the
// proxy server's host name (visible in the core's log) or when the local
// resolver returned an error.
func classify(parent, probeCtx context.Context, err error, timeout time.Duration, tail *tailBuffer) (kind, msg string) {
	if parent.Err() != nil {
		return model.ErrKindCancelled, "cancelled"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if probeCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		if dnsInLog(tail, 0) {
			return model.ErrKindDNS, "proxy server host name could not be resolved"
		}
		return model.ErrKindTimeout, fmt.Sprintf("timeout after %dms", timeout.Milliseconds())
	}
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	text := err.Error()
	switch {
	case errors.As(err, &dnsErr) || strings.Contains(text, "no such host"):
		return model.ErrKindDNS, "DNS resolution failed"
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalid),
		strings.Contains(text, "tls:"), strings.Contains(text, "x509:"):
		return model.ErrKindTLS, "TLS error: " + trimErr(text)
	}
	// Connection-level failures: the core logs the reason a moment after the
	// client sees the reset, so give the log a short chance to show a DNS error.
	connect := func(msg string) (string, string) {
		if dnsInLog(tail, 300*time.Millisecond) {
			return model.ErrKindDNS, "proxy server host name could not be resolved"
		}
		return model.ErrKindConnect, msg
	}
	switch {
	case strings.Contains(text, "connection refused"):
		return connect("connection refused by the proxy chain")
	case strings.Contains(text, "host unreachable"), strings.Contains(text, "network unreachable"):
		return connect("target unreachable through the proxy")
	case strings.Contains(text, "general SOCKS server failure"):
		return connect("proxy outbound failed to connect")
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return connect("tunnel closed by the proxy (server unreachable, remote DNS failure, or credentials/transport mismatch)")
	case errors.Is(err, syscall.ECONNRESET):
		return connect("tunnel reset by the proxy (server unreachable, or credentials/transport mismatch)")
	case strings.Contains(text, "blocked direct connection"):
		return model.ErrKindOther, text
	}
	return model.ErrKindOther, trimErr(localAddrRe.ReplaceAllString(text, ""))
}

// dnsInLog reports whether the core's log mentions a failed name lookup,
// waiting up to wait for it to appear.
func dnsInLog(tail *tailBuffer, wait time.Duration) bool {
	if tail == nil {
		return false
	}
	deadline := time.Now().Add(wait)
	for {
		if dnsLogRe.MatchString(tail.String()) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func trimErr(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// tailBuffer keeps the last max bytes written to it (core output for diagnostics).
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *tailBuffer) summary() string {
	s := strings.Join(strings.Fields(b.String()), " ")
	if s == "" {
		return "no output"
	}
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}
