package proxy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const uid = "b831381d-6324-4d53-ad4f-8cda48b30811"

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func mustParse(t *testing.T, s string) *Node {
	t.Helper()
	n, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return n
}

// Links that mean the same thing must share one Key; the display name must not matter.
func TestKeyIgnoresNameAndOrder(t *testing.T) {
	vmessJSON := func(ps string) string {
		return "vmess://" + b64(`{"v":"2","ps":"`+ps+`","add":"Example.com","port":"443","id":"`+strings.ToUpper(uid)+`","aid":"0","scy":"auto","net":"ws","type":"none","host":"cdn.example.com","path":"/ws","tls":"tls","sni":"cdn.example.com"}`)
	}
	groups := [][]string{
		{
			"vless://" + uid + "@Example.com:443?security=tls&type=ws&path=%2Fws&host=cdn.example.com&sni=cdn.example.com#name-one",
			"vless://" + strings.ToUpper(uid) + "@example.com:443?sni=cdn.example.com&host=cdn.example.com&path=%2Fws&type=ws&security=tls&encryption=none&flow=#another%20name",
			"vless://" + uid + "@example.com:443?type=WS&security=TLS&path=/ws&host=cdn.example.com&sni=cdn.example.com",
		},
		{vmessJSON("one"), vmessJSON("two ✓"), vmessJSON("")},
		{
			"trojan://pa%24s@example.com:443?sni=example.com#a",
			"trojan://pa$s@EXAMPLE.com:443?security=tls&sni=example.com#b",
		},
		{
			"ss://" + b64("aes-256-gcm:pass") + "@example.com:8388#a",
			"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pass")) + "@Example.com:8388/#b",
			"ss://" + b64("aes-256-gcm:pass@example.com:8388") + "#legacy",
			"ss://aes-256-gcm:pass@example.com:8388#plain",
		},
	}
	for gi, g := range groups {
		first := mustParse(t, g[0]).Key
		for _, s := range g[1:] {
			if k := mustParse(t, s).Key; k != first {
				t.Errorf("group %d: keys differ\n  %s\n  %s\n  for %s", gi, first, k, s)
			}
		}
	}
}

func TestDifferentConfigsHaveDifferentKeys(t *testing.T) {
	base := "vless://" + uid + "@example.com:443?security=tls&sni=a.com#x"
	others := []string{
		"vless://" + uid + "@example.com:444?security=tls&sni=a.com#x",       // port
		"vless://" + uid + "@example.org:443?security=tls&sni=a.com#x",       // host
		"vless://" + uid + "@example.com:443?security=tls&sni=b.com#x",       // sni
		"vless://" + uid + "@example.com:443?security=tls&sni=a.com&fp=ios",  // fp
		"vless://" + uid[:35] + "0@example.com:443?security=tls&sni=a.com#x", // id
	}
	k := mustParse(t, base).Key
	for _, o := range others {
		if mustParse(t, o).Key == k {
			t.Errorf("expected different key for %s", o)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"", "hello", "http://example.com", "vless://example.com:443", "vless://" + uid + "@:443",
		"vless://" + uid + "@example.com", "vless://" + uid + "@example.com:99999",
		"vmess://%%%", "vmess://" + b64(`{"add":"x"}`), "ss://zzzz", "trojan://@example.com:1",
	} {
		if n, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) should fail, got %+v", s, n)
		}
	}
}

func TestUnsupportedProtocols(t *testing.T) {
	for _, s := range []string{
		"hysteria2://pw@example.com:443?sni=a.com#h",
		"tuic://" + uid + ":pw@example.com:443#t",
		"ss://" + b64("aes-128-gcm:pw") + "@example.com:8388?plugin=obfs-local%3Bobfs%3Dhttp#p",
		"vmess://" + b64(`{"add":"example.com","port":"443","id":"`+uid+`","net":"quic"}`),
		"vless://" + uid + "@example.com:443?type=h2&security=tls&path=/h&host=example.com",
		"vless://" + uid + "@example.com:443?type=kcp&headerType=wechat-video&seed=abc",
		"vless://" + uid + "@example.com:443?security=reality&sni=a.com", // no public key
	} {
		n := mustParse(t, s)
		if n.Supported() {
			t.Errorf("%s should be unsupported", s)
		}
		if _, err := n.Outbound(); err == nil {
			t.Errorf("%s: Outbound should fail", s)
		}
	}
}

func TestOutboundShapes(t *testing.T) {
	cases := []struct {
		name, uri, network, security string
	}{
		{"vless reality", "vless://" + uid + "@1.2.3.4:443?type=tcp&security=reality&pbk=" + "7fHk1Ntl3p5Yq0m1qZ0m4o3b0m3c5J0b1ZQkVv2o1X8" + "&sid=ab12&sni=www.example.com&fp=chrome&flow=xtls-rprx-vision&spx=%2F#r", "tcp", "reality"},
		{"vless ws tls", "vless://" + uid + "@example.com:443?type=ws&security=tls&path=%2Fws%3Fed%3D2048&host=cdn.example.com#w", "ws", "tls"},
		{"vless grpc", "vless://" + uid + "@example.com:443?type=grpc&security=tls&serviceName=svc&mode=multi&sni=example.com#g", "grpc", "tls"},
		{"vless httpupgrade", "vless://" + uid + "@example.com:443?type=httpupgrade&security=tls&path=/u&host=example.com#h", "httpupgrade", "tls"},
		{"vless xhttp", "vless://" + uid + "@example.com:443?type=xhttp&security=tls&path=/x&host=example.com&mode=auto#x", "xhttp", "tls"},
		{"vless plain tcp", "vless://" + uid + "@example.com:80?type=tcp&security=none&headerType=http&host=a.com&path=/#t", "tcp", "none"},
		{"vless kcp", "vless://" + uid + "@example.com:443?type=kcp#k", "kcp", "none"},
		{"vmess ws", "vmess://" + b64(`{"v":"2","ps":"x","add":"example.com","port":443,"id":"`+uid+`","aid":0,"scy":"auto","net":"ws","host":"example.com","path":"/ws","tls":"tls"}`), "ws", "tls"},
		{"vmess grpc", "vmess://" + b64(`{"add":"example.com","port":"443","id":"`+uid+`","net":"grpc","path":"svc","type":"multi","tls":"tls"}`), "grpc", "tls"},
		{"trojan default tls", "trojan://secret@example.com:443?sni=example.com#t", "tcp", "tls"},
		{"trojan ws", "trojan://secret@example.com:443?type=ws&path=/t&host=example.com&security=tls#t", "ws", "tls"},
		{"ss", "ss://" + b64("chacha20-ietf-poly1305:pw") + "@example.com:8388#s", "tcp", "none"},
		{"ss 2022", "ss://2022-blake3-aes-128-gcm:" + "YctPZ6U7xPPcU%2Bgp3u%2B0tx%2FtRizJN9K8y%2BuKlW2qjlI%3D" + "@example.com:8388#s", "tcp", "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := mustParse(t, c.uri)
			ob, err := n.Outbound()
			if err != nil {
				t.Fatal(err)
			}
			ss := ob["streamSettings"].(map[string]any)
			if ss["network"] != c.network || ss["security"] != c.security {
				t.Errorf("network/security = %v/%v, want %s/%s", ss["network"], ss["security"], c.network, c.security)
			}
			if _, err := json.Marshal(ob); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSNIFallbackToHostForWS(t *testing.T) {
	n := mustParse(t, "vless://"+uid+"@1.2.3.4:443?type=ws&security=tls&host=cdn.example.com&path=/")
	ob, _ := n.Outbound()
	tls := ob["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)
	if tls["serverName"] != "cdn.example.com" {
		t.Errorf("serverName = %v", tls["serverName"])
	}
}

func TestRealityNeedsPublicKey(t *testing.T) {
	n := mustParse(t, "vless://"+uid+"@1.2.3.4:443?security=reality&sni=a.com")
	if _, err := n.Outbound(); err == nil {
		t.Error("expected error without pbk")
	}
}

// With XRAY_BIN set (path of a real xray binary) every generated outbound is
// validated by `xray run -test`, which parses and builds the full config.
func TestOutboundsAcceptedByRealXray(t *testing.T) {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN not set")
	}
	// A valid 32-byte base64url X25519 public key for reality.
	const pbk = "jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0"
	uris := []string{
		"vless://" + uid + "@127.0.0.1:443?type=tcp&security=reality&pbk=" + pbk + "&sid=ab12&sni=www.example.com&fp=chrome&flow=xtls-rprx-vision&spx=%2F",
		"vless://" + uid + "@example.com:443?type=ws&security=tls&path=%2Fws&host=cdn.example.com&alpn=h2,http/1.1&fp=chrome",
		"vless://" + uid + "@example.com:443?type=ws&security=tls&path=%2Fws&host=cdn.example.com&allowInsecure=1",
		"vless://" + uid + "@example.com:443?type=grpc&security=tls&serviceName=svc&mode=multi&sni=example.com",
		"vless://" + uid + "@example.com:443?type=httpupgrade&security=tls&path=/u&host=example.com",
		"vless://" + uid + "@example.com:443?type=xhttp&security=tls&path=/x&host=example.com&mode=auto",
		"vless://" + uid + "@example.com:80?type=tcp&security=none&headerType=http&host=a.com&path=/",
		"vless://" + uid + "@example.com:443?type=kcp",
		"vmess://" + b64(`{"v":"2","ps":"x","add":"example.com","port":443,"id":"`+uid+`","aid":0,"scy":"auto","net":"ws","host":"example.com","path":"/ws","tls":"tls"}`),
		"vmess://" + b64(`{"add":"example.com","port":"443","id":"`+uid+`","net":"grpc","path":"svc","type":"multi","tls":"tls"}`),
		"trojan://secret@example.com:443?sni=example.com",
		"trojan://secret@example.com:443?type=ws&path=/t&host=example.com&security=tls",
		"ss://" + b64("chacha20-ietf-poly1305:pw") + "@example.com:8388",
		"ss://2022-blake3-aes-128-gcm:YctPZ6U7xPPcU%2Bgp3u%2B0tx%2FtRizJN9K8y%2BuKlW2qjlI%3D@example.com:8388",
	}
	dir := t.TempDir()
	for i, u := range uris {
		n := mustParse(t, u)
		ob, err := n.Outbound()
		if err != nil {
			t.Errorf("%s: %v", u, err)
			continue
		}
		cfg := map[string]any{
			"inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": 10808, "protocol": "socks",
				"settings": map[string]any{"auth": "noauth"}}},
			"outbounds": []any{ob},
		}
		data, _ := json.Marshal(cfg)
		f := filepath.Join(dir, "c.json")
		if err := os.WriteFile(f, data, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "run", "-test", "-config", f).CombinedOutput()
		if err != nil {
			t.Errorf("#%d %s\nxray rejected the config: %v\n%s", i, u, err, out)
		}
	}
}
