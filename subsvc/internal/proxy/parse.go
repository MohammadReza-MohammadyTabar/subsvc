// Package proxy parses proxy share links (vless, vmess, trojan, ss, ...),
// computes a normalized identity key used for de-duplication, and converts
// supported links into Xray outbound definitions.
package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxLineLen is the longest share link that will be accepted.
const MaxLineLen = 16 << 10

// Node is a parsed share link.
type Node struct {
	Raw         string            // original link (trimmed)
	Key         string            // normalized identity; the display name is NOT part of it
	Protocol    string            // lower-case scheme
	Name        string            // display name (fragment / ps)
	Host        string            // lower-case host
	Port        int               // 0 when unknown (unsupported protocols only)
	Secret      string            // uuid (vless/vmess), password (trojan/ss)
	Method      string            // ss cipher
	Params      map[string]string // canonical lower-case parameters
	Unsupported string            // non-empty: reason this node cannot be tested
}

// Supported reports whether the node can be converted to an Xray outbound.
func (n *Node) Supported() bool { return n.Unsupported == "" }

var knownSchemes = map[string]bool{
	"vless": true, "vmess": true, "trojan": true, "ss": true,
	"ssr": true, "hysteria": true, "hysteria2": true, "hy2": true,
	"tuic": true, "wireguard": true, "wg": true,
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Parse parses one share link line.
func Parse(line string) (*Node, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(line, "\xef\xbb\xbf"))
	if raw == "" {
		return nil, errors.New("empty line")
	}
	if len(raw) > MaxLineLen {
		return nil, errors.New("line too long")
	}
	i := strings.Index(raw, "://")
	if i <= 0 {
		return nil, errors.New("not a URI")
	}
	scheme := strings.ToLower(raw[:i])
	if !knownSchemes[scheme] {
		return nil, fmt.Errorf("unknown scheme %q", scheme)
	}
	var (
		n   *Node
		err error
	)
	switch scheme {
	case "vmess":
		n, err = parseVMess(raw)
	case "ss":
		n, err = parseSS(raw)
	case "vless", "trojan":
		n, err = parseURLNode(raw, scheme)
	default:
		n, err = parseGeneric(raw, scheme)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", scheme, err)
	}
	n.Raw = raw
	if n.Unsupported == "" { // mark everything the test core cannot run up front
		if _, err := n.Outbound(); err != nil {
			n.Unsupported = err.Error()
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- helpers

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, errors.New("missing port")
	}
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

func unescapeName(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

// hostKey formats a host for use in a key (lower-case, IPv6 in brackets).
func hostKey(h string) string {
	h = strings.ToLower(h)
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

func normSecret(s string) string {
	if uuidRe.MatchString(s) {
		return strings.ToLower(s)
	}
	return s
}

// lowerValueKeys lists parameters whose values are case-insensitive.
var lowerValueKeys = map[string]bool{
	"security": true, "type": true, "encryption": true, "flow": true,
	"headertype": true, "fp": true, "mode": true,
}

// canonParams lower-cases keys, trims values, drops empty values and values
// equal to protocol defaults, and unifies aliases, so that two links that mean
// the same thing produce the same parameter set.
func canonParams(proto string, in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		if lowerValueKeys[k] {
			v = strings.ToLower(v)
		}
		out[k] = v
	}
	if v, ok := out["peer"]; ok { // legacy trojan alias of sni
		if _, has := out["sni"]; !has {
			out["sni"] = v
		}
		delete(out, "peer")
	}
	if v, ok := out["insecure"]; ok {
		if _, has := out["allowinsecure"]; !has {
			out["allowinsecure"] = v
		}
		delete(out, "insecure")
	}
	if v, ok := out["allowinsecure"]; ok {
		if b, err := strconv.ParseBool(v); err == nil && b {
			out["allowinsecure"] = "1"
		} else {
			delete(out, "allowinsecure")
		}
	}
	switch out["type"] {
	case "tcp", "raw":
		delete(out, "type")
	case "h2":
		out["type"] = "http"
	case "splithttp":
		out["type"] = "xhttp"
	case "mkcp":
		out["type"] = "kcp"
	}
	if out["headertype"] == "none" {
		delete(out, "headertype")
	}
	switch proto {
	case "vless":
		if out["security"] == "none" {
			delete(out, "security")
		}
		if out["encryption"] == "none" {
			delete(out, "encryption")
		}
	case "trojan":
		if out["security"] == "tls" {
			delete(out, "security")
		}
	case "vmess":
		if out["security"] == "none" {
			delete(out, "security")
		}
		if out["encryption"] == "auto" {
			delete(out, "encryption")
		}
		if out["aid"] == "0" {
			delete(out, "aid")
		}
	}
	return out
}

func encodeParams(p map[string]string) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[k]))
	}
	return b.String()
}

func queryToMap(rawQuery string) map[string]string {
	vals, _ := url.ParseQuery(rawQuery) // best effort: bad escapes just drop that pair
	m := make(map[string]string, len(vals))
	for k, v := range vals {
		if len(v) > 0 {
			m[k] = v[0]
		}
	}
	return m
}

func buildKey(scheme, secret, host string, port int, path string, params map[string]string) string {
	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	if secret != "" {
		b.WriteString(url.QueryEscape(secret))
		b.WriteByte('@')
	}
	b.WriteString(hostKey(host))
	if port > 0 {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(port))
	}
	if path = strings.TrimRight(path, "/"); path != "" {
		b.WriteString(path)
	}
	if len(params) > 0 {
		b.WriteByte('?')
		b.WriteString(encodeParams(params))
	}
	return b.String()
}

// decodeB64 decodes standard or URL-safe base64, with or without padding.
func decodeB64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, "=")
	if s == "" {
		return nil, errors.New("empty base64")
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// ----------------------------------------------------------- URL-style links

// parseURLNode handles vless:// and trojan:// (and similar user@host:port?query).
func parseURLNode(raw, scheme string) (*Node, error) {
	base, frag, _ := strings.Cut(raw, "#")
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid URI: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("missing host")
	}
	port, err := parsePort(u.Port())
	if err != nil {
		return nil, err
	}
	if u.User == nil {
		return nil, errors.New("missing credentials")
	}
	secret := u.User.Username()
	if pw, ok := u.User.Password(); ok {
		secret += ":" + pw
	}
	if secret == "" {
		return nil, errors.New("empty credentials")
	}
	if scheme == "vless" || scheme == "vmess" {
		secret = normSecret(secret)
	}
	params := canonParams(scheme, queryToMap(u.RawQuery))
	n := &Node{
		Protocol: scheme,
		Name:     unescapeName(frag),
		Host:     strings.ToLower(host),
		Port:     port,
		Secret:   secret,
		Params:   params,
	}
	n.Key = buildKey(scheme, secret, host, port, u.Path, params)
	return n, nil
}

// parseGeneric handles protocols the test core cannot run. They are still
// parsed so they can be de-duplicated and listed.
func parseGeneric(raw, scheme string) (*Node, error) {
	base, frag, _ := strings.Cut(raw, "#")
	n := &Node{
		Protocol:    scheme,
		Name:        unescapeName(frag),
		Unsupported: "protocol " + scheme + " is not supported by the test core",
		Params:      map[string]string{},
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || strings.HasPrefix(scheme, "ssr") {
		payload := strings.TrimSpace(base[len(scheme)+3:])
		if payload == "" {
			return nil, errors.New("empty payload")
		}
		n.Key = scheme + "://" + payload
		return n, nil
	}
	n.Host = strings.ToLower(u.Hostname())
	n.Port, _ = parsePort(u.Port())
	secret := ""
	if u.User != nil {
		secret = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			secret += ":" + pw
		}
	}
	n.Secret = secret
	n.Params = canonParams(scheme, queryToMap(u.RawQuery))
	n.Key = buildKey(scheme, secret, u.Hostname(), n.Port, u.Path, n.Params)
	return n, nil
}

// --------------------------------------------------------------------- vmess

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

func parseVMess(raw string) (*Node, error) {
	payload := raw[len("vmess://"):]
	payload, frag, _ := strings.Cut(payload, "#")

	dec, err := decodeB64(payload)
	if err != nil || !bytes.HasPrefix(bytes.TrimSpace(dec), []byte("{")) {
		if strings.Contains(payload, "@") { // vmess://uuid@host:port?... form
			return parseURLNode(raw, "vmess")
		}
		return nil, errors.New("payload is neither base64 JSON nor a URI")
	}

	d := json.NewDecoder(bytes.NewReader(dec))
	d.UseNumber()
	var obj map[string]any
	if err := d.Decode(&obj); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	f := make(map[string]string, len(obj))
	for k, v := range obj {
		if s := stringify(v); s != "" {
			f[strings.ToLower(k)] = s
		}
	}
	host := strings.ToLower(f["add"])
	if host == "" {
		return nil, errors.New("missing address")
	}
	port, err := parsePort(f["port"])
	if err != nil {
		return nil, err
	}
	id := normSecret(f["id"])
	if id == "" {
		return nil, errors.New("missing id")
	}
	name := f["ps"]
	if name == "" {
		name = unescapeName(frag)
	}

	net := strings.ToLower(f["net"])
	if net == "quic" {
		// handled below as unsupported
		net = "quic"
	}
	params := map[string]string{
		"type":       net,
		"headertype": f["type"],
		"host":       f["host"],
		"path":       f["path"],
		"security":   f["tls"],
		"sni":        f["sni"],
		"alpn":       f["alpn"],
		"fp":         f["fp"],
		"encryption": f["scy"],
		"aid":        f["aid"],
	}
	if v := f["allowinsecure"]; v != "" {
		params["allowinsecure"] = v
	}
	if net == "grpc" {
		params["servicename"] = f["path"]
		delete(params, "path")
		if params["headertype"] == "multi" {
			params["mode"] = "multi"
		}
		delete(params, "headertype")
	}
	params = canonParams("vmess", params)

	n := &Node{
		Protocol: "vmess",
		Name:     name,
		Host:     host,
		Port:     port,
		Secret:   id,
		Params:   params,
	}
	if net == "quic" {
		n.Unsupported = "vmess over quic is not supported by the test core"
	}
	// Key: scheme + canonical, sorted fields of the JSON (without the name "ps").
	key := map[string]string{"add": host, "port": strconv.Itoa(port), "id": id}
	for k, v := range params {
		key[k] = v
	}
	kb, _ := json.Marshal(key) // map keys are sorted by encoding/json
	n.Key = "vmess://" + string(kb)
	return n, nil
}

// ------------------------------------------------------------------ shadowsocks

func parseSS(raw string) (*Node, error) {
	rest := raw[len("ss://"):]
	rest, frag, _ := strings.Cut(rest, "#")
	rest, query, _ := strings.Cut(rest, "?")
	rest = strings.TrimRight(rest, "/")

	var method, password, hostport string
	if at := strings.LastIndex(rest, "@"); at >= 0 { // SIP002: userinfo@host:port
		userinfo, hp := rest[:at], rest[at+1:]
		hostport = hp
		if dec, err := url.PathUnescape(userinfo); err == nil {
			userinfo = dec
		}
		if !strings.Contains(userinfo, ":") {
			b, err := decodeB64(userinfo)
			if err != nil {
				return nil, errors.New("cannot decode userinfo")
			}
			userinfo = string(b)
		}
		m, p, ok := strings.Cut(userinfo, ":")
		if !ok {
			return nil, errors.New("userinfo must be method:password")
		}
		method, password = m, p
	} else { // legacy: base64(method:password@host:port)
		b, err := decodeB64(rest)
		if err != nil {
			return nil, errors.New("cannot decode legacy payload")
		}
		s := string(b)
		at := strings.LastIndex(s, "@")
		if at < 0 {
			return nil, errors.New("legacy payload has no @")
		}
		hostport = s[at+1:]
		m, p, ok := strings.Cut(s[:at], ":")
		if !ok {
			return nil, errors.New("legacy payload must be method:password@host:port")
		}
		method, password = m, p
	}

	host, portStr, err := net.SplitHostPort(strings.TrimSpace(hostport))
	if err != nil {
		return nil, fmt.Errorf("invalid host:port: %w", err)
	}
	if host == "" {
		return nil, errors.New("missing host")
	}
	port, err := parsePort(portStr)
	if err != nil {
		return nil, err
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" || password == "" {
		return nil, errors.New("missing method or password")
	}

	params := canonParams("ss", queryToMap(query))
	n := &Node{
		Protocol: "ss",
		Name:     unescapeName(frag),
		Host:     strings.ToLower(host),
		Port:     port,
		Secret:   password,
		Method:   method,
		Params:   params,
	}
	if p := params["plugin"]; p != "" {
		n.Unsupported = "shadowsocks plugin " + strconv.Quote(strings.SplitN(p, ";", 2)[0]) + " is not supported by the test core"
	}
	n.Key = buildKey("ss", method+":"+password, host, port, "", params)
	return n, nil
}
