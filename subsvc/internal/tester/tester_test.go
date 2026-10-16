package tester

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsvc/internal/proxy"
)

// ------------------------------------------------------------------------
// The test binary doubles as a fake proxy core: when SUBSVC_FAKE_CORE=1 it
// behaves like `xray run -config file` and serves a SOCKS5 listener whose
// behaviour depends on the outbound address of the config. That lets the unit
// tests exercise the real process/SOCKS/HTTP path without Xray installed.
// ------------------------------------------------------------------------

func TestMain(m *testing.M) {
	if os.Getenv("SUBSVC_FAKE_CORE") == "1" {
		fakeCore()
		return
	}
	os.Exit(m.Run())
}

func fakeCore() {
	file := os.Args[len(os.Args)-1]
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var cfg struct {
		Inbounds []struct {
			Port int `json:"port"`
		} `json:"inbounds"`
		Outbounds []struct {
			Settings struct {
				Vnext   []struct{ Address string } `json:"vnext"`
				Servers []struct{ Address string } `json:"servers"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	addr := ""
	ob := cfg.Outbounds[0].Settings
	if len(ob.Vnext) > 0 {
		addr = ob.Vnext[0].Address
	} else if len(ob.Servers) > 0 {
		addr = ob.Servers[0].Address
	}
	if addr == "crash.test" {
		fmt.Fprintln(os.Stderr, "Failed to start: bad config")
		os.Exit(23)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Inbounds[0].Port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go socksServe(c, addr)
	}
}

func socksServe(c net.Conn, mode string) {
	defer c.Close()
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(c, buf[:int(buf[1])]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	var host string
	switch buf[3] {
	case 1:
		io.ReadFull(c, buf[:4])
		host = net.IP(buf[:4]).String()
	case 3:
		io.ReadFull(c, buf[:1])
		l := int(buf[0])
		io.ReadFull(c, buf[:l])
		host = string(buf[:l])
	case 4:
		io.ReadFull(c, buf[:16])
		host = net.IP(buf[:16]).String()
	}
	io.ReadFull(c, buf[:2])
	port := binary.BigEndian.Uint16(buf[:2])

	switch mode {
	case "deny.test": // the proxy refuses to connect
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	case "slow.test": // the proxy accepts but never answers
		time.Sleep(10 * time.Second)
		return
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 2*time.Second)
	if err != nil {
		c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, c)
	io.Copy(c, up)
}

// ------------------------------------------------------------------ tests

const uid = "b831381d-6324-4d53-ad4f-8cda48b30811"

func target(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/generate_204", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/ok200", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/generate_204", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func fakeTester(t *testing.T, targetURL string, timeout time.Duration) *Tester {
	t.Helper()
	t.Setenv("SUBSVC_FAKE_CORE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{CorePath: exe, URL: targetURL, Timeout: timeout,
		StartupTimeout: 5 * time.Second, WorkDir: t.TempDir()})
}

func node(t *testing.T, host string) *proxy.Node {
	t.Helper()
	n, err := proxy.Parse("vless://" + uid + "@" + host + ":443?security=none#x")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSuccessThroughProxy(t *testing.T) {
	tt := fakeTester(t, target(t)+"/generate_204", 3*time.Second)
	r := tt.Test(context.Background(), node(t, "ok.test"))
	if !r.Tested || !r.Reachable || r.StatusCode != 204 || r.Error != "" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if r.LatencyMS < r.ConnectMS || r.LatencyMS < r.ResponseMS {
		t.Errorf("latency must be the total: %+v", r)
	}
	if r.LatencyMS < r.ConnectMS+r.ResponseMS-1 { // allow 1ms rounding
		t.Errorf("latency (%d) should equal connect (%d) + response (%d)", r.LatencyMS, r.ConnectMS, r.ResponseMS)
	}
	if r.TestedAt == nil {
		t.Error("TestedAt not set")
	}
}

// The target is reachable directly; the only reason for failure is that the
// "proxy" refuses. This proves the request really goes through the proxy.
func TestProxyDenyIsFailureEvenIfTargetIsUp(t *testing.T) {
	tt := fakeTester(t, target(t)+"/generate_204", 3*time.Second)
	r := tt.Test(context.Background(), node(t, "deny.test"))
	if !r.Tested || r.Reachable || r.Error == "" {
		t.Fatalf("expected failure, got %+v", r)
	}
	if r.LatencyMS != 0 {
		t.Errorf("failed test must not report latency: %+v", r)
	}
}

func TestTimeout(t *testing.T) {
	tt := fakeTester(t, target(t)+"/generate_204", 400*time.Millisecond)
	start := time.Now()
	r := tt.Test(context.Background(), node(t, "slow.test"))
	if r.Reachable || !strings.Contains(r.Error, "timeout after 400ms") {
		t.Fatalf("expected timeout, got %+v", r)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout not enforced: %v", time.Since(start))
	}
}

func TestNon204IsFailure(t *testing.T) {
	base := target(t)
	for path, want := range map[string]string{"/ok200": "200", "/redirect": "302"} {
		tt := fakeTester(t, base+path, 3*time.Second)
		r := tt.Test(context.Background(), node(t, "ok.test"))
		if r.Reachable || r.StatusCode == 0 || !strings.Contains(r.Error, want) {
			t.Errorf("%s: got %+v", path, r)
		}
	}
}

func TestCoreCrashIsReported(t *testing.T) {
	tt := fakeTester(t, target(t)+"/generate_204", 3*time.Second)
	r := tt.Test(context.Background(), node(t, "crash.test"))
	if r.Reachable || !strings.Contains(r.Error, "core exited") || !strings.Contains(r.Error, "bad config") {
		t.Fatalf("got %+v", r)
	}
}

func TestMissingCore(t *testing.T) {
	tt := New(Options{CorePath: "/nonexistent/xray", URL: "http://127.0.0.1:1/", Timeout: time.Second,
		StartupTimeout: time.Second, WorkDir: t.TempDir()})
	r := tt.Test(context.Background(), node(t, "ok.test"))
	if r.Reachable || !strings.Contains(r.Error, "start core") {
		t.Fatalf("got %+v", r)
	}
}

func TestUnsupportedNodeIsNotTested(t *testing.T) {
	tt := fakeTester(t, "http://127.0.0.1:1/", time.Second)
	n, err := proxy.Parse("hysteria2://pw@example.com:443#h")
	if err != nil {
		t.Fatal(err)
	}
	r := tt.Test(context.Background(), n)
	if r.Tested || r.Reachable || r.Error == "" {
		t.Fatalf("got %+v", r)
	}
}

func TestCancelStopsTest(t *testing.T) {
	tt := fakeTester(t, target(t)+"/generate_204", 8*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := tt.Test(ctx, node(t, "slow.test"))
	if r.Reachable || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel not honoured: %+v after %v", r, time.Since(start))
	}
}

func TestNoLeftoverFiles(t *testing.T) {
	tt := fakeTester(t, target(t)+"/generate_204", 3*time.Second)
	tt.Test(context.Background(), node(t, "ok.test"))
	tt.Test(context.Background(), node(t, "crash.test"))
	left, _ := filepath.Glob(filepath.Join(tt.opts.WorkDir, "*"))
	if len(left) != 0 {
		t.Errorf("credential files left behind: %v", left)
	}
}

// ------------------------------------------------------------------------
// Real Xray end-to-end test (set XRAY_BIN to run it): a second Xray instance
// acts as the remote proxy server, the tester reaches a local HTTP 204 server
// through it. A wrong password must fail.
// ------------------------------------------------------------------------

func realXray(t *testing.T) string {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN not set")
	}
	return bin
}

func startXrayServer(t *testing.T, bin string, inbound map[string]any) {
	t.Helper()
	cfg := map[string]any{
		"log":       map[string]any{"loglevel": "warning"},
		"inbounds":  []any{inbound},
		"outbounds": []any{map[string]any{"protocol": "freedom"}},
	}
	data, _ := json.Marshal(cfg)
	f := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(f, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCmd(bin, f)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%v", inbound["port"])
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("xray server did not start")
}

func TestRealXrayEndToEnd(t *testing.T) {
	bin := realXray(t)
	targetURL := target(t) + "/generate_204"
	tt := New(Options{CorePath: bin, URL: targetURL, Timeout: 5 * time.Second,
		StartupTimeout: 5 * time.Second, WorkDir: t.TempDir()})

	ssPort, vlessPort := mustFreePort(t), mustFreePort(t)
	startXrayServer(t, bin, map[string]any{
		"listen": "127.0.0.1", "port": ssPort, "protocol": "shadowsocks",
		"settings": map[string]any{"method": "chacha20-ietf-poly1305", "password": "secret-pw", "network": "tcp"},
	})
	startXrayServer(t, bin, map[string]any{
		"listen": "127.0.0.1", "port": vlessPort, "protocol": "vless",
		"settings": map[string]any{"clients": []any{map[string]any{"id": uid}}, "decryption": "none"},
	})

	cases := []struct {
		name, uri string
		want      bool
	}{
		{"ss ok", fmt.Sprintf("ss://chacha20-ietf-poly1305:secret-pw@127.0.0.1:%d#ok", ssPort), true},
		{"ss wrong password", fmt.Sprintf("ss://chacha20-ietf-poly1305:WRONG@127.0.0.1:%d#bad", ssPort), false},
		{"vless ok", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=none&type=tcp#ok", uid, vlessPort), true},
		{"vless wrong id", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=none&type=tcp#bad", "00000000-0000-4000-8000-000000000000", vlessPort), false},
		{"nothing listening", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=none#dead", uid, mustFreePort(t)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := proxy.Parse(c.uri)
			if err != nil {
				t.Fatal(err)
			}
			r := tt.Test(context.Background(), n)
			t.Logf("result: %+v", r)
			if r.Reachable != c.want {
				t.Fatalf("reachable = %v, want %v (%+v)", r.Reachable, c.want, r)
			}
			if c.want && (r.StatusCode != 204 || r.LatencyMS <= 0) {
				t.Errorf("bad success result: %+v", r)
			}
			if !c.want && r.Error == "" {
				t.Errorf("failure without error: %+v", r)
			}
		})
	}
}

func mustFreePort(t *testing.T) int {
	t.Helper()
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
