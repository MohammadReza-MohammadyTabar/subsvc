#!/usr/bin/env python3
import http.server, socketserver, urllib.request, urllib.error, os, json, re
import urllib.parse

BACKEND = os.environ.get("SUBSC_BACKEND", "http://127.0.0.1:8080")
OUTPUT_NAME = os.environ.get("SUBSC_OUTPUT_NAME", "").strip()
OUTPUT_NUMBERED = os.environ.get("SUBSC_OUTPUT_NUMBERED", "true").lower() in ("1","true","yes")
PORT = 9090
STATIC = os.path.dirname(os.path.abspath(__file__))

def load_token():
    t = os.environ.get("SUBSC_TOKEN", "").strip()
    if t: return t
    for path in ("/etc/subsvc/config.json", os.path.expanduser("~/.subsvc.json")):
        try:
            with open(path) as f:
                t = (json.load(f).get("auth_token") or "").strip()
                if t: return t
        except Exception:
            pass
    return ""

TOKEN = load_token()

class H(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=STATIC, **kw)

    def _backend(self, method, path, body=None, token=None, extra_headers=None):
        url = BACKEND + path
        headers = {}
        tok = token if token is not None else TOKEN
        if tok:
            headers["Authorization"] = "Bearer " + tok
        if extra_headers:
            headers.update(extra_headers)
        req = urllib.request.Request(url, data=body, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=300) as r:
                return r.status, dict(r.headers), r.read()
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers or {}), e.read()
        except Exception as e:
            return 502, {"Content-Type": "application/json"}, json.dumps({"error": f"proxy: {e}"}).encode()

    def _send(self, status, headers, body, ctype=None):
        self.send_response(status)
        for k, v in headers.items():
            if k.lower() in ("transfer-encoding", "connection", "content-length"):
                continue
            self.send_header(k, v)
        if ctype:
            self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/sub" or self.path.startswith("/sub?"):
            qs = self.path[5:]
            parts = ["format=base64"]
            if OUTPUT_NAME:
                parts.append("name=" + urllib.parse.quote(OUTPUT_NAME))
                if OUTPUT_NUMBERED:
                    parts.append("numbered=true")
            if qs.startswith("?"):
                parts.append(qs[1:])
            path = "/api/output?" + "&".join(parts)
            s, h, b = self._backend("GET", path)
            return self._send(s, h, b, "text/plain; charset=utf-8")
        m = re.match(r"^/sub/([a-f0-9]+)$", self.path)
        if m:
            s, h, b = self._backend("GET", f"/api/output?format=base64&link={m.group(1)}")
            return self._send(s, h, b, "text/plain; charset=utf-8")
        if self.path.startswith("/api/") or self.path == "/healthz":
            tok = None
            auth = self.headers.get("Authorization", "")
            if auth.startswith("Bearer "): tok = auth[7:]
            s, h, b = self._backend("GET", self.path, token=tok)
            return self._send(s, h, b)
        if self.path in ("/", ""):
            self.path = "/subsvc-ui.html"
        return super().do_GET()

    def _proxy(self, method):
        n = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(n) if n else None
        tok = None
        auth = self.headers.get("Authorization", "")
        if auth.startswith("Bearer "): tok = auth[7:]
        s, h, b = self._backend(method, self.path, body, token=tok)
        return self._send(s, h, b)

    def do_POST(self):   self._proxy("POST")
    def do_PUT(self):    self._proxy("PUT")
    def do_DELETE(self): self._proxy("DELETE")

    def log_message(self, *a): pass

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

with Server(("0.0.0.0", PORT), H) as s:
    print(f"UI: http://0.0.0.0:{PORT}")
    print(f"Subscription: http://0.0.0.0:{PORT}/sub")
    if not TOKEN:
        print("WARN: auth token پیدا نشد — /sub کار نمی‌کنه اگه subsvc توکن داشته باشه")
    s.serve_forever()
