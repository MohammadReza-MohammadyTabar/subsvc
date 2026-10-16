package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"subsvc/internal/model"
	"subsvc/internal/proxy"
	"subsvc/internal/service"
	"subsvc/internal/store"
	"subsvc/internal/subscription"
)

const uid = "b831381d-6324-4d53-ad4f-8cda48b30811"

// fakeProber: hosts starting with "good" are reachable and the port is the
// latency in ms. It records the maximum number of concurrent tests.
type fakeProber struct {
	mu   sync.Mutex
	gate chan struct{}

	cur, max atomic.Int32
	calls    atomic.Int32
}

func (p *fakeProber) setGate(g chan struct{}) { p.mu.Lock(); p.gate = g; p.mu.Unlock() }

func (p *fakeProber) Test(ctx context.Context, n *proxy.Node) model.TestResult {
	c := p.cur.Add(1)
	defer p.cur.Add(-1)
	for {
		m := p.max.Load()
		if c <= m || p.max.CompareAndSwap(m, c) {
			break
		}
	}
	p.calls.Add(1)
	p.mu.Lock()
	g := p.gate
	p.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return model.TestResult{Tested: true, Error: "cancelled"}
		}
	}
	now := time.Now().UTC()
	r := model.TestResult{Tested: true, TestedAt: &now}
	if strings.HasPrefix(n.Host, "good") {
		r.Reachable, r.StatusCode, r.LatencyMS = true, 204, int64(n.Port)
	} else {
		r.Error = "timeout after 5000ms"
	}
	return r
}

type env struct {
	t     *testing.T
	dir   string
	st    *store.Store
	svc   *service.Service
	srv   *httptest.Server
	probe *fakeProber
	subs  *httptest.Server
	subA  atomic.Value // current body of /a
	failA atomic.Bool
}

func newEnv(t *testing.T, token string) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), probe: &fakeProber{}}
	a := base64.StdEncoding.EncodeToString([]byte(strings.Join([]string{
		"vless://" + uid + "@good1.example.com:300?security=tls&sni=a.com#A-name",
		"vless://" + uid + "@good2.example.com:100?security=tls&sni=a.com#two",
		"trojan://pw@bad1.example.com:443?sni=b.com#bad",
	}, "\n")))
	e.subA.Store(a)
	b := strings.Join([]string{
		"vless://" + uid + "@good1.example.com:300?sni=a.com&security=tls#B-name", // duplicate of A's first
		"ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw")) + "@good3.example.com:200#ss",
		"hysteria2://pw@hy.example.com:443#hy",
	}, "\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		if e.failA.Load() {
			http.Error(w, "boom", 500)
			return
		}
		w.Write([]byte(e.subA.Load().(string)))
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(b)) })
	e.subs = httptest.NewServer(mux)
	t.Cleanup(e.subs.Close)
	e.open(token)
	return e
}

func (e *env) open(token string) {
	st, err := store.Open(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	e.st = st
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.svc = service.New(log, st, subscription.NewFetcher(time.Second, 1<<20, "test"), e.probe,
		service.Options{FetchConcurrency: 2, TestConcurrency: 2})
	e.srv = httptest.NewServer(New(e.svc, log, token))
	e.t.Cleanup(e.srv.Close)
}

func (e *env) do(method, path string, body any, token string) (int, []byte, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp.Header
}

func (e *env) addLink(name, path string) model.Link {
	e.t.Helper()
	code, body, _ := e.do("POST", "/api/links", map[string]string{"name": name, "url": e.subs.URL + path}, "")
	if code != 201 {
		e.t.Fatalf("add link: %d %s", code, body)
	}
	var l model.Link
	json.Unmarshal(body, &l)
	return l
}

func (e *env) waitDone() service.Job {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, body, _ := e.do("GET", "/api/status", nil, "")
		var st service.Status
		json.Unmarshal(body, &st)
		if st.Job != nil && st.Job.State != "running" {
			return *st.Job
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("job did not finish")
	return service.Job{}
}

func (e *env) output(query string) []string {
	e.t.Helper()
	code, body, _ := e.do("GET", "/api/output"+query, nil, "")
	if code != 200 {
		e.t.Fatalf("output: %d %s", code, body)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func hostsOf(t *testing.T, lines []string) []string {
	var hs []string
	for _, l := range lines {
		n, err := proxy.Parse(l)
		if err != nil {
			t.Fatalf("output line is not a valid config: %q", l)
		}
		hs = append(hs, n.Host)
	}
	return hs
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t, "")
	a := e.addLink("A", "/a")
	b := e.addLink("B", "/b")

	// validation / duplicates
	if code, _, _ := e.do("POST", "/api/links", map[string]string{"url": e.subs.URL + "/a"}, ""); code != 409 {
		t.Errorf("duplicate link: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/links", map[string]string{"url": "ftp://x"}, ""); code != 400 {
		t.Errorf("bad scheme: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/links", map[string]any{"url": "http://x", "bogus": 1}, ""); code != 400 {
		t.Errorf("unknown field: %d", code)
	}

	// Nothing is fetched automatically.
	time.Sleep(100 * time.Millisecond)
	if n := len(e.st.Entries("")); n != 0 {
		t.Fatalf("configs appeared without a refresh request: %d", n)
	}
	if got := e.output(""); len(got) != 0 {
		t.Fatalf("output must be empty before any test: %v", got)
	}

	// Refresh while a test is blocked: second request must be rejected.
	gate := make(chan struct{})
	e.probe.setGate(gate)
	if code, body, _ := e.do("POST", "/api/refresh", nil, ""); code != 202 {
		t.Fatalf("refresh: %d %s", code, body)
	}
	if code, _, _ := e.do("POST", "/api/refresh", nil, ""); code != 409 {
		t.Errorf("concurrent refresh must be 409, got %d", code)
	}
	if code, _, _ := e.do("POST", "/api/test", nil, ""); code != 409 {
		t.Errorf("concurrent test must be 409, got %d", code)
	}
	close(gate)
	job := e.waitDone()
	if job.State != "done" || job.LinksDone != 2 || job.LinksFail != 0 {
		t.Fatalf("job: %+v", job)
	}
	// 5 unique configs: A has 3, B adds ss + hysteria2 (its first line duplicates A).
	// hysteria2 is unsupported and never tested.
	if job.TestTotal != 4 || job.TestDone != 4 || job.TestOK != 3 {
		t.Errorf("test counters: %+v", job)
	}
	if m := e.probe.max.Load(); m > 2 {
		t.Errorf("more than 2 concurrent tests: %d", m)
	}

	out := e.output("")
	if got := strings.Join(hostsOf(t, out), ","); got != "good2.example.com,good3.example.com,good1.example.com" {
		t.Errorf("output hosts (fastest first, reachable only, deduplicated) = %s", got)
	}
	code, body, hdr := e.do("GET", "/api/output?format=base64", nil, "")
	dec, err := base64.StdEncoding.DecodeString(string(body))
	if code != 200 || err != nil || hdr.Get("X-Config-Count") != "3" || strings.Count(string(dec), "\n") != 3 {
		t.Errorf("base64 output: %d %v count=%s", code, err, hdr.Get("X-Config-Count"))
	}
	if got := e.output("?limit=1"); len(got) != 1 {
		t.Errorf("limit: %v", got)
	}
	// per-link filter: link B contains good1 (duplicate) and good3
	if got := strings.Join(hostsOf(t, e.output("?link="+b.ID)), ","); got != "good3.example.com,good1.example.com" {
		t.Errorf("link filter = %s", got)
	}

	// The first occurrence keeps its link/name; the duplicate is stored once with two sources.
	for _, c := range e.st.Entries("") {
		if c.Host == "good1.example.com" && len(c.Sources) != 2 {
			t.Errorf("good1 sources = %v", c.Sources)
		}
		if c.Protocol == "hysteria2" && (c.Unsupported == "" || c.Test.Tested) {
			t.Errorf("hysteria2 must be unsupported and untested: %+v", c)
		}
	}

	// A broken source must not stop anything nor wipe old data.
	e.failA.Store(true)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	if code, _, _ := e.do("POST", "/api/links", map[string]string{"name": "dead", "url": deadURL}, ""); code != 201 {
		t.Fatal("add dead link")
	}
	if code, _, _ := e.do("POST", "/api/refresh?test=false", nil, ""); code != 202 {
		t.Fatal("refresh 2")
	}
	job = e.waitDone()
	if job.State != "done" || job.LinksDone != 3 || job.LinksFail != 2 || len(job.Errors) != 2 {
		t.Errorf("job 2: %+v", job)
	}
	_, body, _ = e.do("GET", "/api/links/"+a.ID, nil, "")
	var la model.Link
	json.Unmarshal(body, &la)
	if la.LastFetch == nil || la.LastFetch.OK || !strings.Contains(la.LastFetch.Error, "HTTP 500") {
		t.Errorf("link A status: %+v", la.LastFetch)
	}
	if got := e.output(""); len(got) != 3 {
		t.Errorf("stale data must be kept when a source fails, output = %v", got)
	}

	// Persistence: a new process (re-open) sees links, configs and test results.
	e.srv.Close()
	e.open("")
	if got := e.output(""); len(got) != 3 {
		t.Errorf("after restart output = %v", got)
	}
	if len(e.st.Links()) != 3 {
		t.Errorf("links after restart: %d", len(e.st.Links()))
	}

	// Deleting link B removes only configs that came exclusively from it.
	if code, _, _ := e.do("DELETE", "/api/links/"+b.ID, nil, ""); code != 204 {
		t.Fatal("delete")
	}
	if got := strings.Join(hostsOf(t, e.output("")), ","); got != "good2.example.com,good1.example.com" {
		t.Errorf("after delete: %s", got)
	}
	if code, _, _ := e.do("GET", "/api/links/"+b.ID, nil, ""); code != 404 {
		t.Errorf("deleted link: %d", code)
	}
}

func TestOnlyUntestedAndRetest(t *testing.T) {
	e := newEnv(t, "")
	e.addLink("A", "/a")
	e.do("POST", "/api/refresh?test=false", nil, "")
	e.waitDone()
	if e.probe.calls.Load() != 0 {
		t.Fatal("test=false must not run tests")
	}
	e.do("POST", "/api/test", nil, "")
	e.waitDone()
	first := e.probe.calls.Load()
	if first != 3 {
		t.Fatalf("calls = %d", first)
	}
	e.do("POST", "/api/test?only_untested=true", nil, "")
	e.waitDone()
	if e.probe.calls.Load() != first {
		t.Error("only_untested re-tested configs")
	}
	e.do("POST", "/api/test", nil, "")
	e.waitDone()
	if e.probe.calls.Load() != 2*first {
		t.Error("full test must re-test everything")
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, "")
	e.addLink("A", "/a")
	e.probe.setGate(make(chan struct{})) // never released
	e.do("POST", "/api/refresh", nil, "")
	for i := 0; i < 100 && e.probe.cur.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if code, _, _ := e.do("POST", "/api/cancel", nil, ""); code != 202 {
		t.Fatalf("cancel: %d", code)
	}
	if job := e.waitDone(); job.State != "cancelled" {
		t.Errorf("job: %+v", job)
	}
	if code, _, _ := e.do("POST", "/api/cancel", nil, ""); code != 409 {
		t.Errorf("cancel with nothing running: %d", code)
	}
	if got := e.output(""); len(got) != 0 {
		t.Errorf("cancelled test must not produce output: %v", got)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, "s3cret")
	for _, tok := range []string{"", "wrong", "s3cre", "S3CRET"} {
		if code, _, _ := e.do("GET", "/api/links", nil, tok); code != 401 {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	if code, _, _ := e.do("GET", "/api/links", nil, "s3cret"); code != 200 {
		t.Errorf("valid token: %d", code)
	}
	if code, _, _ := e.do("GET", "/healthz", nil, ""); code != 200 {
		t.Errorf("healthz must not need a token: %d", code)
	}
}

func TestUpdateLink(t *testing.T) {
	e := newEnv(t, "")
	a := e.addLink("A", "/a")
	e.addLink("B", "/b")
	off := false
	code, body, _ := e.do("PUT", "/api/links/"+a.ID, map[string]any{"name": "renamed", "enabled": off}, "")
	var l model.Link
	json.Unmarshal(body, &l)
	if code != 200 || l.Name != "renamed" || l.Enabled {
		t.Fatalf("update: %d %s", code, body)
	}
	// disabled links are skipped by a general refresh
	e.do("POST", "/api/refresh?test=false", nil, "")
	job := e.waitDone()
	if job.LinksTotal != 1 {
		t.Errorf("links in refresh: %d", job.LinksTotal)
	}
	// but can be refreshed explicitly
	e.do("POST", "/api/refresh?test=false&link="+a.ID, nil, "")
	job = e.waitDone()
	if job.LinksTotal != 1 || job.LinksDone != 1 {
		t.Errorf("explicit refresh: %+v", job)
	}
	if code, _, _ := e.do("PUT", "/api/links/"+a.ID, map[string]any{"url": e.subs.URL + "/b"}, ""); code != 409 {
		t.Errorf("update to duplicate url: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/refresh?link=nope", nil, ""); code != 404 {
		t.Errorf("unknown link: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/refresh?test=maybe", nil, ""); code != 400 {
		t.Errorf("bad bool: %d", code)
	}
}
