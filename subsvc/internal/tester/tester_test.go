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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"subsvc/internal/model"
	"subsvc/internal/proxy"
	"subsvc/internal/settings"
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
			Listen string `json:"listen"`
			Port   int    `json:"port"`
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
	if dump := os.Getenv("SUBSVC_FAKE_DUMP"); dump != "" {
		_ = os.WriteFile(dump, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Inbounds[0].Listen, cfg.Inbounds[0].Port))
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

	if dp := os.Getenv("SUBSVC_FAKE_DENY_PORT"); dp != "" && dp == fmt.Sprint(port) {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // this destination is blocked by the "proxy"
		return
	}
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

// ------------------------------------------------------------------ helpers

const uid = "b831381d-6324-4d53-ad4f-8cda48b30811"

// world is a set of local HTTP servers standing in for "the internet".
type world struct {
	general *httptest.Server // generate_204 + misc
	youtube *httptest.Server // generate_204
	speed   atomic.Int32     // concurrent /speed requests right now
	speedMx atomic.Int32     // max concurrent /speed requests seen
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{}
	g := http.NewServeMux()
	g.HandleFunc("/generate_204", func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(204) })
	g.HandleFunc("/ok200", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("hi")) })
	g.HandleFunc("/redirect", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/generate_204", http.StatusFound)
	})
	g.HandleFunc("/speed", func(rw http.ResponseWriter, r *http.Request) {
		c := w.speed.Add(1)
		defer w.speed.Add(-1)
		for {
			m := w.speedMx.Load()
			if c <= m || w.speedMx.CompareAndSwap(m, c) {
				break
			}
		}
		var n int
		fmt.Sscan(r.URL.Query().Get("bytes"), &n)
		switch r.URL.Query().Get("mode") {
		case "500":
			http.Error(rw, "no", 500)
			return
		case "stall": // 50 KB, then nothing
			rw.Write(make([]byte, 50<<10))
			rw.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		case "slow": // hold the request so concurrency can be observed
			time.Sleep(150 * time.Millisecond)
		}
		chunk := make([]byte, 16<<10)
		for sent := 0; sent < n; sent += len(chunk) {
			rw.Write(chunk)
		}
	})
	w.general = httptest.NewServer(g)
	y := http.NewServeMux()
	y.HandleFunc("/generate_204", func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(204) })
	w.youtube = httptest.NewServer(y)
	t.Cleanup(func() { w.general.Close(); w.youtube.Close() })
	return w
}

func (w *world) port(s *httptest.Server) string { return s.URL[strings.LastIndex(s.URL, ":")+1:] }

func (w *world) settings() settings.Settings {
	s := settings.Default()
	s.GeneralURL = w.general.URL + "/generate_204"
	s.YouTubeURL = w.youtube.URL + "/generate_204"
	s.SpeedURL = w.general.URL + "/speed?bytes=" + settings.SpeedPlaceholder
	s.SpeedMaxBytes = 256 << 10
	s.TimeoutMS = 3000
	s.SpeedTimeoutMS = 5000
	return s
}

func fakeTester(t *testing.T, extraEnv ...string) *Tester {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{CorePath: exe, WorkDir: t.TempDir(),
		ExtraEnv: append([]string{"SUBSVC_FAKE_CORE=1"}, extraEnv...)})
}

func node(t *testing.T, host string) *proxy.Node {
	t.Helper()
	n, err := proxy.Parse("vless://" + uid + "@" + host + ":443?security=none#x")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func run(tt *Tester, n *proxy.Node, s settings.Settings) model.TestResult {
	return tt.Test(context.Background(), n, s, NewGate(2))
}

// ------------------------------------------------------------------ tests

func TestAllThreeSucceedThroughProxy(t *testing.T) {
	w := newWorld(t)
	r := run(fakeTester(t), node(t, "ok.test"), w.settings())
	if !r.Tested || r.TestedAt == nil {
		t.Fatalf("not tested: %+v", r)
	}
	g, y, sp := r.General, r.YouTube, r.Speed
	if g == nil || y == nil || sp == nil {
		t.Fatalf("missing results: %+v", r)
	}
	for name, p := range map[string]*model.Probe{"general": g, "youtube": y} {
		if !p.OK || p.StatusCode != 204 || p.Error != "" || p.ErrorKind != "" {
			t.Errorf("%s: %+v", name, p)
		}
		if p.LatencyMS < p.ConnectMS || p.LatencyMS < p.ResponseMS || p.LatencyMS < p.ConnectMS+p.ResponseMS-1 {
			t.Errorf("%s latency must be connect+response: %+v", name, p)
		}
	}
	if !sp.Tested || !sp.OK || sp.Bytes != 256<<10 || sp.Mbps <= 0 || sp.DurationMS < 0 {
		t.Errorf("speed: %+v", sp)
	}
	// the flat legacy fields mirror the general probe
	if !r.Reachable || r.LatencyMS != g.LatencyMS || r.StatusCode != 204 {
		t.Errorf("legacy mirror broken: %+v", r)
	}
}

// The target servers are reachable directly. The only reason for failure is
// that the "proxy" refuses, which proves the requests really go through it.
func TestProxyDenyIsFailureEvenIfTargetIsUp(t *testing.T) {
	w := newWorld(t)
	r := run(fakeTester(t), node(t, "deny.test"), w.settings())
	for name, p := range map[string]*model.Probe{"general": r.General, "youtube": r.YouTube} {
		if p == nil || p.OK || p.Error == "" || p.ErrorKind != model.ErrKindConnect || p.LatencyMS != 0 {
			t.Errorf("%s: %+v", name, p)
		}
	}
	if r.Speed == nil || r.Speed.Tested || r.Speed.Skipped == "" {
		t.Errorf("speed must be skipped for a dead config: %+v", r.Speed)
	}
	if r.Reachable || r.Error == "" {
		t.Errorf("legacy mirror: %+v", r)
	}
}

// The three tests are independent: YouTube blocked while the general internet works.
func TestYouTubeFailsIndependently(t *testing.T) {
	w := newWorld(t)
	tt := fakeTester(t, "SUBSVC_FAKE_DENY_PORT="+w.port(w.youtube))

	s := w.settings()
	r := run(tt, node(t, "ok.test"), s)
	if !r.General.OK || r.YouTube.OK || r.YouTube.ErrorKind != model.ErrKindConnect {
		t.Fatalf("general must pass and youtube fail: %+v / %+v", r.General, r.YouTube)
	}
	if !r.Reachable {
		t.Error("legacy reachable must follow the general probe")
	}
	if r.Speed == nil || r.Speed.Tested || !strings.Contains(r.Speed.Skipped, "criteria") {
		t.Errorf("speed must be skipped because YouTube is required: %+v", r.Speed)
	}
	if s.Eligible(r) {
		t.Error("a config without YouTube must not be eligible when YouTube is required")
	}

	s.RequireYouTube = false // criteria changed: the speed test now runs, config becomes eligible
	r = run(tt, node(t, "ok.test"), s)
	if r.Speed == nil || !r.Speed.OK || !s.Eligible(r) {
		t.Errorf("with youtube optional: speed=%+v eligible=%v", r.Speed, s.Eligible(r))
	}
}

func TestGeneralFailsYouTubeWorks(t *testing.T) {
	w := newWorld(t)
	tt := fakeTester(t, "SUBSVC_FAKE_DENY_PORT="+w.port(w.general))
	s := w.settings()
	s.SpeedEnabled = false
	r := run(tt, node(t, "ok.test"), s)
	if r.General.OK || !r.YouTube.OK || r.Speed != nil {
		t.Fatalf("%+v %+v %+v", r.General, r.YouTube, r.Speed)
	}
	s.RequireGeneral = false
	if !s.Eligible(r) {
		t.Error("with require_general=false a YouTube-only config is eligible")
	}
}

func TestYouTubeDisabledIsNotTested(t *testing.T) {
	w := newWorld(t)
	s := w.settings()
	s.YouTubeEnabled, s.RequireYouTube, s.SpeedEnabled = false, false, false
	r := run(fakeTester(t), node(t, "ok.test"), s)
	if r.YouTube != nil || r.Speed != nil || !r.General.OK {
		t.Errorf("%+v", r)
	}
}

func TestTimeoutIsParallelAndClassified(t *testing.T) {
	w := newWorld(t)
	s := w.settings()
	s.TimeoutMS = 400
	start := time.Now()
	r := run(fakeTester(t), node(t, "slow.test"), s)
	for name, p := range map[string]*model.Probe{"general": r.General, "youtube": r.YouTube} {
		if p.OK || p.ErrorKind != model.ErrKindTimeout || !strings.Contains(p.Error, "timeout after 400ms") {
			t.Errorf("%s: %+v", name, p)
		}
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("the two probes must run in parallel (took %v)", d)
	}
}

func TestNon204IsFailure(t *testing.T) {
	w := newWorld(t)
	for path, code := range map[string]int{"/ok200": 200, "/redirect": 302} {
		s := w.settings()
		s.GeneralURL = w.general.URL + path
		s.SpeedEnabled = false
		r := run(fakeTester(t), node(t, "ok.test"), s)
		g := r.General
		if g.OK || g.StatusCode != code || g.ErrorKind != model.ErrKindHTTP || !strings.Contains(g.Error, fmt.Sprint(code)) {
			t.Errorf("%s: %+v", path, g)
		}
		if !r.YouTube.OK {
			t.Errorf("%s: youtube must be unaffected: %+v", path, r.YouTube)
		}
	}
}

func TestTLSErrorIsClassified(t *testing.T) {
	w := newWorld(t)
	bad := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(204) }))
	defer bad.Close()
	s := w.settings()
	s.YouTubeURL = bad.URL + "/generate_204" // self-signed certificate: untrusted
	s.SpeedEnabled = false
	r := run(fakeTester(t), node(t, "ok.test"), s)
	if r.YouTube.OK || r.YouTube.ErrorKind != model.ErrKindTLS || !strings.HasPrefix(r.YouTube.Error, "TLS error") {
		t.Errorf("youtube: %+v", r.YouTube)
	}
	if !r.General.OK {
		t.Errorf("general must be unaffected: %+v", r.General)
	}
}

func TestSpeedRespectsMaxBytes(t *testing.T) {
	w := newWorld(t)
	s := w.settings()
	s.SpeedURL = w.general.URL + "/speed?bytes=5000000" // the server offers 5 MB
	s.SpeedMaxBytes = 128 << 10
	r := run(fakeTester(t), node(t, "ok.test"), s)
	if r.Speed == nil || !r.Speed.OK || r.Speed.Bytes != 128<<10 {
		t.Errorf("must stop at max bytes: %+v", r.Speed)
	}
}

func TestSpeedErrors(t *testing.T) {
	w := newWorld(t)
	s := w.settings()

	s.SpeedURL = w.general.URL + "/speed?mode=500&bytes=" + settings.SpeedPlaceholder
	r := run(fakeTester(t), node(t, "ok.test"), s)
	if sp := r.Speed; sp == nil || sp.OK || sp.ErrorKind != model.ErrKindHTTP {
		t.Errorf("500: %+v", sp)
	}
	if !r.General.OK || !r.YouTube.OK {
		t.Error("a failing speed test must not affect the probes")
	}

	s.SpeedURL = w.general.URL + "/speed?mode=stall&bytes=" + settings.SpeedPlaceholder
	s.SpeedTimeoutMS = 800
	r = run(fakeTester(t), node(t, "ok.test"), s)
	if sp := r.Speed; sp == nil || sp.OK || sp.ErrorKind != model.ErrKindTimeout || sp.Bytes != 50<<10 {
		t.Errorf("stall must time out with the partial byte count: %+v", sp)
	}
}

func TestSpeedGateSerializesDownloads(t *testing.T) {
	w := newWorld(t)
	s := w.settings()
	s.SpeedURL = w.general.URL + "/speed?mode=slow&bytes=" + settings.SpeedPlaceholder
	tt := fakeTester(t)
	gate := NewGate(1)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := tt.Test(context.Background(), node(t, "ok.test"), s, gate); r.Speed == nil || !r.Speed.OK {
				t.Errorf("speed: %+v", r.Speed)
			}
		}()
	}
	wg.Wait()
	if m := w.speedMx.Load(); m != 1 {
		t.Errorf("speed_concurrency=1 but %d downloads ran at once", m)
	}
}

func TestGuardedDialBlocksDirectConnections(t *testing.T) {
	d := guardedDial("127.0.0.1:1")
	if _, err := d(context.Background(), "tcp", "127.0.0.1:80"); err == nil || !strings.Contains(err.Error(), "blocked direct connection") {
		t.Fatalf("direct dial must be refused, got %v", err)
	}
	if _, err := d(context.Background(), "tcp", "www.google.com:443"); err == nil {
		t.Fatal("direct dial must be refused")
	}
}

func TestCoreCrashIsReported(t *testing.T) {
	w := newWorld(t)
	r := run(fakeTester(t), node(t, "crash.test"), w.settings())
	if r.Reachable || r.General == nil || !strings.Contains(r.General.Error, "core exited") || !strings.Contains(r.General.Error, "bad config") {
		t.Fatalf("got %+v", r)
	}
	if r.YouTube != nil || r.Speed != nil {
		t.Errorf("nothing else may be tested without a core: %+v", r)
	}
}

func TestMissingCore(t *testing.T) {
	w := newWorld(t)
	tt := New(Options{CorePath: "/nonexistent/xray", WorkDir: t.TempDir()})
	r := run(tt, node(t, "ok.test"), w.settings())
	if r.Reachable || r.General == nil || !strings.Contains(r.General.Error, "start core") {
		t.Fatalf("got %+v", r)
	}
}

func TestUnsupportedNodeIsNotTested(t *testing.T) {
	w := newWorld(t)
	n, err := proxy.Parse("hysteria2://pw@example.com:443#h")
	if err != nil {
		t.Fatal(err)
	}
	r := run(fakeTester(t), n, w.settings())
	if r.Tested || r.Reachable || r.Error == "" || r.General != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestCancelStopsTestAndCleansUp(t *testing.T) {
	w := newWorld(t)
	s := w.settings()
	s.TimeoutMS = 8000
	tt := fakeTester(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := tt.Test(ctx, node(t, "slow.test"), s, NewGate(1))
	if r.General != nil && r.General.OK || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel not honoured: %+v after %v", r, time.Since(start))
	}
	if left, _ := filepath.Glob(filepath.Join(tt.opts.WorkDir, "*")); len(left) != 0 {
		t.Errorf("files left behind after cancel: %v", left)
	}
}

func TestNoLeftoverFilesAndNoSecretsInCoreEnv(t *testing.T) {
	w := newWorld(t)
	dump := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("SUBSVC_AUTH_TOKEN", "super-secret-token-value")
	tt := fakeTester(t, "SUBSVC_FAKE_DUMP="+dump)
	run(tt, node(t, "ok.test"), w.settings())
	run(tt, node(t, "crash.test"), w.settings())
	if left, _ := filepath.Glob(filepath.Join(tt.opts.WorkDir, "*")); len(left) != 0 {
		t.Errorf("credential files left behind: %v", left)
	}
	env, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "super-secret") || strings.Contains(string(env), "SUBSVC_AUTH_TOKEN") {
		t.Error("the core process must not inherit the auth token")
	}
	for _, want := range []string{"GOMAXPROCS=1", "GOMEMLIMIT="} {
		if !strings.Contains(string(env), want) {
			t.Errorf("core env lacks %s", want)
		}
	}
}

// ------------------------------------------------------------------------
// Real Xray end-to-end test (set XRAY_BIN to run it). Second Xray instances
// act as the remote proxy servers; the tester reaches local HTTP servers
// THROUGH them. One server blocks the "youtube" port in its own routing, which
// shows the independent YouTube verdict with a real core.
// ------------------------------------------------------------------------

func realXray(t *testing.T) string {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN not set")
	}
	return bin
}

func startXrayServer(t *testing.T, bin string, inbound map[string]any, blockPort string) {
	t.Helper()
	cfg := map[string]any{
		"log":      map[string]any{"loglevel": "warning"},
		"inbounds": []any{inbound},
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom"},
			map[string]any{"tag": "block", "protocol": "blackhole"},
		},
	}
	if blockPort != "" {
		cfg["routing"] = map[string]any{"rules": []any{
			map[string]any{"type": "field", "port": blockPort, "outboundTag": "block"},
		}}
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
	w := newWorld(t)
	tt := New(Options{CorePath: bin, WorkDir: t.TempDir()})

	ssPort, vlessPort, ytBlockedPort := mustFreePort(t), mustFreePort(t), mustFreePort(t)
	startXrayServer(t, bin, map[string]any{
		"listen": "127.0.0.1", "port": ssPort, "protocol": "shadowsocks",
		"settings": map[string]any{"method": "chacha20-ietf-poly1305", "password": "secret-pw", "network": "tcp"},
	}, "")
	startXrayServer(t, bin, map[string]any{
		"listen": "127.0.0.1", "port": vlessPort, "protocol": "vless",
		"settings": map[string]any{"clients": []any{map[string]any{"id": uid}}, "decryption": "none"},
	}, "")
	startXrayServer(t, bin, map[string]any{ // this server cannot reach the "youtube" port
		"listen": "127.0.0.1", "port": ytBlockedPort, "protocol": "vless",
		"settings": map[string]any{"clients": []any{map[string]any{"id": uid}}, "decryption": "none"},
	}, w.port(w.youtube))

	type want struct{ general, youtube, speed bool }
	cases := []struct {
		name string
		uri  string
		want want
		kind string // expected error kind of the general probe when it must fail
	}{
		{"ss ok", fmt.Sprintf("ss://chacha20-ietf-poly1305:secret-pw@127.0.0.1:%d#ok", ssPort), want{true, true, true}, ""},
		{"vless ok", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=none&type=tcp#ok", uid, vlessPort), want{true, true, true}, ""},
		{"youtube blocked by that server", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=none&type=tcp#ytblocked", uid, ytBlockedPort), want{true, false, false}, ""},
		{"ss wrong password", fmt.Sprintf("ss://chacha20-ietf-poly1305:WRONG@127.0.0.1:%d#bad", ssPort), want{}, model.ErrKindTimeout},
		{"vless wrong id", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=none&type=tcp#bad", "00000000-0000-4000-8000-000000000000", vlessPort), want{}, model.ErrKindConnect},
		{"nothing listening", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=none#dead", uid, mustFreePort(t)), want{}, model.ErrKindConnect},
		{"unresolvable server host", fmt.Sprintf("vless://%s@no-such-host.invalid:443?security=none#dns", uid), want{}, model.ErrKindDNS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := proxy.Parse(c.uri)
			if err != nil {
				t.Fatal(err)
			}
			s := w.settings()
			s.TimeoutMS = 2500
			r := run(tt, n, s)
			t.Logf("general=%+v youtube=%+v speed=%+v", r.General, r.YouTube, r.Speed)
			if r.General == nil || r.General.OK != c.want.general {
				t.Fatalf("general = %+v, want ok=%v", r.General, c.want.general)
			}
			if r.YouTube == nil || r.YouTube.OK != c.want.youtube {
				t.Fatalf("youtube = %+v, want ok=%v", r.YouTube, c.want.youtube)
			}
			speedOK := r.Speed != nil && r.Speed.OK
			if speedOK != c.want.speed {
				t.Fatalf("speed = %+v, want ok=%v", r.Speed, c.want.speed)
			}
			if c.want.general && (r.General.StatusCode != 204 || r.General.LatencyMS <= 0 && r.General.ConnectMS+r.General.ResponseMS < 0) {
				t.Errorf("bad success result: %+v", r.General)
			}
			if c.want.speed && (r.Speed.Bytes != 256<<10 || r.Speed.Mbps <= 0) {
				t.Errorf("bad speed result: %+v", r.Speed)
			}
			if !c.want.general && (r.General.Error == "" || r.General.ErrorKind != c.kind) {
				t.Errorf("failure kind = %q (%s), want %q", r.General.ErrorKind, r.General.Error, c.kind)
			}
		})
	}
}

func mustFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
