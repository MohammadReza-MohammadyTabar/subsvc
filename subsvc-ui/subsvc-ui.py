#!/usr/bin/env python3
import http.server, socketserver, urllib.request, urllib.error, os, json, re
import urllib.parse

def env(name, legacy=None, default=""):
    """SUBSVC_UI_* first, then the legacy SUBSC_* name, then the default."""
    for n in (name, legacy):
        if n and os.environ.get(n, "").strip():
            return os.environ[n].strip()
    return default

BACKEND = env("SUBSVC_UI_BACKEND", "SUBSC_BACKEND", "http://127.0.0.1:8080")
OUTPUT_NAME = env("SUBSVC_UI_OUTPUT_NAME", "SUBSC_OUTPUT_NAME")
OUTPUT_NUMBERED = env("SUBSVC_UI_OUTPUT_NUMBERED", "SUBSC_OUTPUT_NUMBERED", "true").lower() in ("1", "true", "yes")
PORT = int(env("SUBSVC_UI_PORT", None, "9090"))
STATIC = os.path.dirname(os.path.abspath(__file__))

# Token used ONLY for the public /sub endpoints (subscription clients cannot
# send headers). Every /api/* request uses the token the browser sends, so the
# UI port never grants API access by itself. The token comes from the same
# variable the backend uses; there is no config.json involved.
TOKEN = env("SUBSVC_AUTH_TOKEN", "SUBSC_TOKEN")

SECURITY_HEADERS = {
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
    "Cache-Control": "no-store",
    "Content-Security-Policy": "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'",
}

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

    def _browser_token(self):
        """The token the BROWSER sent. Never falls back to the server's own
        token: the UI port must not grant API access to anonymous callers."""
        auth = self.headers.get("Authorization", "")
        return auth[7:].strip() if auth.startswith("Bearer ") else ""

    def _send(self, status, headers, body, ctype=None):
        self.send_response(status)
        for k, v in headers.items():
            if k.lower() in ("transfer-encoding", "connection", "content-length"):
                continue
            self.send_header(k, v)
        if ctype:
            self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in SECURITY_HEADERS.items():
            self.send_header(k, v)
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
            s, h, b = self._backend("GET", self.path, token=self._browser_token())
            return self._send(s, h, b)
        if self.path in ("/", "", "/subsvc-ui.html"):
            with open(os.path.join(STATIC, "subsvc-ui.html"), "rb") as f:
                return self._send(200, {}, f.read(), "text/html; charset=utf-8")
        return self._send(404, {}, b"not found", "text/plain; charset=utf-8")

    def _proxy(self, method):
        n = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(n) if n else None
        s, h, b = self._backend(method, self.path, body, token=self._browser_token())
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
        print("WARN: SUBSVC_AUTH_TOKEN is not set for the UI container: /sub will fail if the backend requires a token")
    s.serve_forever()
