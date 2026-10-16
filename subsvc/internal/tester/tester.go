// Package tester performs a real connectivity test of one config: it starts
// the proxy core (Xray) with that single config, exposes it as a local SOCKS5
// listener, sends an HTTPS request to the test URL THROUGH that listener and
// requires HTTP 204. The core only lives for the duration of one test.
package tester

import (
	"context"
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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"subsvc/internal/model"
	"subsvc/internal/proxy"
)

// Options configures a Tester.
type Options struct {
	CorePath       string        // xray binary
	URL            string        // test URL, expects 204
	Timeout        time.Duration // per-config probe timeout
	StartupTimeout time.Duration // time for the core to start listening
	WorkDir        string        // directory for short-lived private config files
}

// Tester runs real tests. It holds no state and is safe for concurrent use.
type Tester struct {
	opts Options
}

// New returns a Tester.
func New(o Options) *Tester { return &Tester{opts: o} }

// Test checks one node. It never panics and always returns a result.
func (t *Tester) Test(ctx context.Context, n *proxy.Node) model.TestResult {
	res := model.TestResult{}
	ob, err := n.Outbound()
	if err != nil {
		res.Error = err.Error() // Tested stays false: nothing was attempted
		return res
	}
	res.Tested = true
	now := time.Now().UTC()
	res.TestedAt = &now

	var port int
	var startErr error
	for attempt := 0; attempt < 2; attempt++ { // one retry covers a lost port race
		var stop func()
		port, stop, startErr = t.startCore(ctx, ob)
		if startErr == nil {
			t.probe(ctx, port, &res)
			stop()
			if !res.Reachable && n.Params["allowinsecure"] == "1" {
				res.Error += " (the link sets allowInsecure, which current Xray no longer supports)"
			}
			return res
		}
		// Only an early core exit can be a lost port race; anything else is final.
		if ctx.Err() != nil || !strings.HasPrefix(startErr.Error(), "core exited") {
			break
		}
	}
	res.Error = startErr.Error()
	if ctx.Err() != nil {
		res.Tested = false
		res.TestedAt = nil
	}
	return res
}

// startCore launches the core listening on a free local port and waits until
// the port accepts connections. stop terminates the core and removes files.
func (t *Tester) startCore(ctx context.Context, ob map[string]any) (port int, stop func(), err error) {
	port, err = freePort()
	if err != nil {
		return 0, nil, fmt.Errorf("no free local port: %w", err)
	}
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{map[string]any{
			"tag": "in", "listen": "127.0.0.1", "port": port, "protocol": "socks",
			"settings": map[string]any{"auth": "noauth", "udp": false},
		}},
		"outbounds": []any{ob},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return 0, nil, err
	}
	// The file holds credentials: private directory (0700) and file (0600),
	// removed as soon as the core stops.
	dir, err := os.MkdirTemp(t.opts.WorkDir, "run-*")
	if err != nil {
		return 0, nil, fmt.Errorf("create work dir: %w", err)
	}
	file := dir + "/config.json"
	if err := os.WriteFile(file, data, 0o600); err != nil {
		os.RemoveAll(dir)
		return 0, nil, fmt.Errorf("write core config: %w", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, t.opts.CorePath, "run", "-config", file)
	tail := &tailBuffer{max: 2048}
	cmd.Stdout, cmd.Stderr = tail, tail
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(dir)
		return 0, nil, fmt.Errorf("start core: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop = func() {
		cancel() // CommandContext kills the process
		<-exited
		os.RemoveAll(dir)
	}

	addr := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.NewTimer(t.opts.StartupTimeout)
	defer deadline.Stop()
	for {
		select {
		case <-exited:
			os.RemoveAll(dir)
			return 0, nil, fmt.Errorf("core exited: %s", tail.summary())
		case <-ctx.Done():
			stop()
			return 0, nil, errors.New("cancelled")
		case <-deadline.C:
			stop()
			return 0, nil, fmt.Errorf("core did not start within %s: %s", t.opts.StartupTimeout, tail.summary())
		default:
		}
		if c, derr := net.DialTimeout("tcp", addr, 150*time.Millisecond); derr == nil {
			c.Close()
			select { // the listener might belong to someone else if the core already died
			case <-exited:
				os.RemoveAll(dir)
				return 0, nil, fmt.Errorf("core exited: %s", tail.summary())
			default:
			}
			return port, stop, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// probe sends the HTTP request through the SOCKS5 listener of the core.
func (t *Tester) probe(ctx context.Context, port int, res *model.TestResult) {
	proxyURL := &url.URL{Scheme: "socks5", Host: "127.0.0.1:" + strconv.Itoa(port)}
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(proxyURL), // ALL traffic goes through the config's tunnel
		DisableKeepAlives:     true,
		DisableCompression:    true,
		TLSHandshakeTimeout:   t.opts.Timeout,
		ResponseHeaderTimeout: t.opts.Timeout,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		Timeout:   t.opts.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a redirect is not a 204
		},
	}

	pctx, cancel := context.WithTimeout(ctx, t.opts.Timeout)
	defer cancel()

	var start, gotConn time.Time
	trace := &httptrace.ClientTrace{
		// Fires once the tunnel to the target (SOCKS CONNECT through the proxy
		// outbound, plus TLS for https) is established.
		GotConn: func(httptrace.GotConnInfo) { gotConn = time.Now() },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(pctx, trace), http.MethodGet, t.opts.URL, nil)
	if err != nil {
		res.Error = "invalid test URL"
		return
	}
	req.Header.Set("User-Agent", "subsvc-probe/1.0")

	start = time.Now()
	resp, err := client.Do(req)
	end := time.Now()
	if !gotConn.IsZero() {
		res.ConnectMS = gotConn.Sub(start).Milliseconds()
	}
	if err != nil {
		res.Error = describe(ctx, pctx, err, t.opts.Timeout)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	res.StatusCode = resp.StatusCode
	if resp.StatusCode != http.StatusNoContent {
		res.Error = "unexpected HTTP status " + strconv.Itoa(resp.StatusCode)
		return
	}
	res.Reachable = true
	res.LatencyMS = end.Sub(start).Milliseconds()
	if !gotConn.IsZero() {
		res.ResponseMS = end.Sub(gotConn).Milliseconds()
	}
}

func describe(parent, probeCtx context.Context, err error, timeout time.Duration) string {
	if parent.Err() != nil {
		return "cancelled"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if probeCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Sprintf("timeout after %dms", timeout.Milliseconds())
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "tunnel closed by the proxy (server unreachable, or credentials/transport mismatch)"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "tunnel reset by the proxy (server unreachable, or credentials/transport mismatch)"
	}
	// Drop the ephemeral local SOCKS address pair; it carries no information.
	return localAddrRe.ReplaceAllString(err.Error(), "")
}

var localAddrRe = regexp.MustCompile(`tcp 127\.0\.0\.1:\d+->`)

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
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

func (b *tailBuffer) summary() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.Join(strings.Fields(string(b.buf)), " ")
	if s == "" {
		return "no output"
	}
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}
