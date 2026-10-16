package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstCSV(s string) string {
	if l := splitCSV(s); len(l) > 0 {
		return l[0]
	}
	return ""
}

// Outbound converts the node into an Xray outbound object (tag "proxy").
func (n *Node) Outbound() (map[string]any, error) {
	if !n.Supported() {
		return nil, errors.New(n.Unsupported)
	}
	stream, err := n.streamSettings()
	if err != nil {
		return nil, err
	}
	ob := map[string]any{"tag": "proxy", "protocol": n.Protocol, "streamSettings": stream}
	if n.Protocol == "ss" {
		ob["protocol"] = "shadowsocks" // Xray's name for the protocol
	}
	p := n.Params

	switch n.Protocol {
	case "vless":
		user := map[string]any{"id": n.Secret, "encryption": "none"}
		if v := p["encryption"]; v != "" {
			user["encryption"] = v
		}
		if v := p["flow"]; v != "" {
			user["flow"] = v
		}
		ob["settings"] = map[string]any{"vnext": []any{map[string]any{
			"address": n.Host, "port": n.Port, "users": []any{user},
		}}}
	case "vmess":
		aid, _ := strconv.Atoi(p["aid"])
		sec := p["encryption"]
		if sec == "" {
			sec = "auto"
		}
		ob["settings"] = map[string]any{"vnext": []any{map[string]any{
			"address": n.Host, "port": n.Port,
			"users": []any{map[string]any{"id": n.Secret, "alterId": aid, "security": sec}},
		}}}
	case "trojan":
		srv := map[string]any{"address": n.Host, "port": n.Port, "password": n.Secret}
		if v := p["flow"]; v != "" {
			srv["flow"] = v
		}
		ob["settings"] = map[string]any{"servers": []any{srv}}
	case "ss":
		ob["settings"] = map[string]any{"servers": []any{map[string]any{
			"address": n.Host, "port": n.Port, "method": n.Method, "password": n.Secret,
		}}}
	default:
		return nil, fmt.Errorf("unsupported protocol %q", n.Protocol)
	}
	return ob, nil
}

func (n *Node) streamSettings() (map[string]any, error) {
	p := n.Params
	network := p["type"]
	if network == "" {
		network = "tcp"
	}
	ss := map[string]any{}

	host, path := p["host"], p["path"]
	switch network {
	case "tcp":
		if p["headertype"] == "http" {
			if path == "" {
				path = "/"
			}
			req := map[string]any{"path": splitCSV(path)}
			if host != "" {
				req["headers"] = map[string]any{"Host": splitCSV(host)}
			}
			ss["tcpSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": req}}
		}
	case "ws":
		ws := map[string]any{}
		if path != "" {
			ws["path"] = path
		}
		if host != "" {
			ws["headers"] = map[string]any{"Host": host}
		}
		ss["wsSettings"] = ws
	case "grpc":
		g := map[string]any{"serviceName": p["servicename"], "multiMode": p["mode"] == "multi"}
		if v := p["authority"]; v != "" {
			g["authority"] = v
		}
		ss["grpcSettings"] = g
	case "httpupgrade":
		h := map[string]any{}
		if path != "" {
			h["path"] = path
		}
		if host != "" {
			h["host"] = host
		}
		ss["httpupgradeSettings"] = h
	case "xhttp":
		x := map[string]any{}
		if path != "" {
			x["path"] = path
		}
		if host != "" {
			x["host"] = host
		}
		if v := p["mode"]; v != "" {
			x["mode"] = v
		}
		if v := p["extra"]; v != "" {
			var extra any
			if err := json.Unmarshal([]byte(v), &extra); err == nil {
				x["extra"] = extra
			}
		}
		ss["xhttpSettings"] = x
	case "http":
		// Removed from current Xray releases (migrated to XHTTP), so a legacy
		// h2 config cannot be run by the test core.
		return nil, errors.New("transport http (h2) was removed from current Xray releases")
	case "kcp":
		if p["headertype"] != "" || p["seed"] != "" {
			return nil, errors.New("mKCP header/seed (finalmask) is not supported by the test core")
		}
		ss["kcpSettings"] = map[string]any{}
	default:
		return nil, fmt.Errorf("transport %q is not supported by the test core", network)
	}
	ss["network"] = network

	security := p["security"]
	if security == "" && n.Protocol == "trojan" {
		security = "tls"
	}
	sni := p["sni"]
	if sni == "" && host != "" && (network == "ws" || network == "httpupgrade" || network == "xhttp") {
		sni = firstCSV(host) // same fallback that common clients use
	}
	switch security {
	case "", "none":
		ss["security"] = "none"
	case "tls":
		t := map[string]any{}
		if sni != "" {
			t["serverName"] = sni
		}
		if v := p["fp"]; v != "" {
			t["fingerprint"] = v
		}
		if v := p["alpn"]; v != "" {
			t["alpn"] = splitCSV(v)
		}
		// allowInsecure is intentionally NOT emitted: current Xray rejects the
		// field. Such configs are tested with normal certificate verification.
		ss["security"] = "tls"
		ss["tlsSettings"] = t
	case "reality":
		if p["pbk"] == "" {
			return nil, fmt.Errorf("reality requires pbk (public key)")
		}
		fp := p["fp"]
		if fp == "" {
			fp = "chrome"
		}
		r := map[string]any{"fingerprint": fp, "publicKey": p["pbk"]}
		if sni != "" {
			r["serverName"] = sni
		}
		if v := p["sid"]; v != "" {
			r["shortId"] = v
		}
		if v := p["spx"]; v != "" {
			r["spiderX"] = v
		}
		ss["security"] = "reality"
		ss["realitySettings"] = r
	default:
		return nil, fmt.Errorf("security %q is not supported by the test core", security)
	}
	return ss, nil
}
