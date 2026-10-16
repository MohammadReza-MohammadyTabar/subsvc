package subscription

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const uid = "b831381d-6324-4d53-ad4f-8cda48b30811"

var links = []string{
	"vless://" + uid + "@a.example.com:443?security=tls&type=ws&path=%2F#A",
	"trojan://pw@b.example.com:443?sni=b.example.com#B",
	"ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw")) + "@c.example.com:8388#C",
}

func plain() string { return strings.Join(links, "\n") + "\n" }

func TestDecodeVariants(t *testing.T) {
	raw := []byte(plain())
	std := base64.StdEncoding.EncodeToString(raw)
	cases := map[string]string{
		"plain":            plain(),
		"plain crlf":       strings.ReplaceAll(plain(), "\n", "\r\n"),
		"plain bom":        "\xef\xbb\xbf" + plain(),
		"base64 std":       std,
		"base64 unpadded":  strings.TrimRight(std, "="),
		"base64 url":       base64.URLEncoding.EncodeToString(raw),
		"base64 url raw":   base64.RawURLEncoding.EncodeToString(raw),
		"base64 wrapped":   wrap(std, 64),
		"base64 crlf wrap": strings.ReplaceAll(wrap(std, 76), "\n", "\r\n"),
		"base64 spaces":    "  " + std + "  \n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res := Parse([]byte(body))
			if len(res.Nodes) != 3 || res.Invalid != 0 {
				t.Fatalf("nodes=%d invalid=%d total=%d", len(res.Nodes), res.Invalid, res.Total)
			}
		})
	}
}

func wrap(s string, n int) string {
	var b strings.Builder
	for len(s) > n {
		b.WriteString(s[:n] + "\n")
		s = s[n:]
	}
	b.WriteString(s)
	return b.String()
}

func TestParseCountsAndDedupe(t *testing.T) {
	body := "# profile-title: test\n" +
		links[0] + "\n" +
		strings.Replace(links[0], "#A", "#renamed", 1) + "\n" + // same config, other name
		"vless://" + uid + "@a.example.com:443?path=%2F&type=ws&security=tls\n" + // reordered query
		"garbage line with://but-no-scheme-we-know\n" +
		"vless://broken\n\n" +
		links[1] + "\n" +
		"hysteria2://pw@d.example.com:443#H\n"
	res := Parse([]byte(body))
	if len(res.Nodes) != 3 {
		t.Fatalf("unique nodes = %d, want 3", len(res.Nodes))
	}
	if res.Duplicates != 2 {
		t.Errorf("duplicates = %d, want 2", res.Duplicates)
	}
	if res.Invalid != 2 {
		t.Errorf("invalid = %d, want 2", res.Invalid)
	}
	if res.Total != 7 {
		t.Errorf("total = %d, want 7", res.Total)
	}
	if res.Nodes[0].Name != "A" {
		t.Errorf("first occurrence must win, got name %q", res.Nodes[0].Name)
	}
}

func TestParseGarbage(t *testing.T) {
	for _, body := range []string{"", "   \n", "<html><body>Forbidden</body></html>", "!!!not base64!!!", "aGVsbG8="} {
		if res := Parse([]byte(body)); len(res.Nodes) != 0 {
			t.Errorf("Parse(%q) found %d nodes", body, len(res.Nodes))
		}
	}
}

func newFetcher() *Fetcher { return NewFetcher(1*time.Second, 64<<10, "test") }

func TestFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "test" {
			http.Error(w, "ua", 400)
			return
		}
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(plain()))))
	})
	mux.HandleFunc("/forbidden", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) })
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>login</html>")) })
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", 100<<10)))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := newFetcher()

	res, err := f.Fetch(context.Background(), srv.URL+"/ok")
	if err != nil || len(res.Nodes) != 3 || res.HTTPStatus != 200 {
		t.Fatalf("ok: %v %+v", err, res)
	}
	for path, want := range map[string]string{
		"/forbidden": "HTTP 403",
		"/html":      "no valid configs",
		"/empty":     "empty response",
		"/huge":      "larger than",
		"/slow":      "timeout after 1s",
	} {
		_, err := f.Fetch(context.Background(), srv.URL+path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", path, err, want)
		}
	}

	// Connection refused: the error must not leak the (secret) URL.
	srv2 := httptest.NewServer(http.NotFoundHandler())
	dead := srv2.URL + "/sub?token=SECRET"
	srv2.Close()
	_, err = f.Fetch(context.Background(), dead)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), dead) {
		t.Errorf("dead server error leaks URL or is nil: %v", err)
	}
}
