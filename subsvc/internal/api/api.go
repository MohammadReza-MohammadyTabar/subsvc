// Package api exposes the HTTP interface (JSON + plain text) using only net/http.
package api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"subsvc/internal/service"
	"subsvc/internal/store"
)

// Server holds the handlers.
type Server struct {
	svc   *service.Service
	log   *slog.Logger
	token string
}

// New returns the HTTP handler. An empty token disables authentication.
func New(svc *service.Service, log *slog.Logger, token string) http.Handler {
	s := &Server{svc: svc, log: log, token: token}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /api/status", s.status)

	mux.HandleFunc("GET /api/links", s.listLinks)
	mux.HandleFunc("POST /api/links", s.addLink)
	mux.HandleFunc("GET /api/links/{id}", s.getLink)
	mux.HandleFunc("PUT /api/links/{id}", s.updateLink)
	mux.HandleFunc("DELETE /api/links/{id}", s.deleteLink)

	mux.HandleFunc("POST /api/refresh", s.refresh)
	mux.HandleFunc("POST /api/test", s.test)
	mux.HandleFunc("POST /api/cancel", s.cancel)

	mux.HandleFunc("GET /api/configs", s.configs)
	mux.HandleFunc("DELETE /api/configs", s.deleteConfigsBulk)
	mux.HandleFunc("DELETE /api/configs/{key...}", s.deleteConfig)
	mux.HandleFunc("GET /api/output", s.output)

	return s.middleware(mux)
}

// ------------------------------------------------------------- middleware

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("handler panic", "path", r.URL.Path, "panic", rec)
				if sw.code == http.StatusOK {
					writeError(sw, http.StatusInternalServerError, "internal error")
				}
			}
			if r.URL.Path != "/healthz" {
				// Reads (status polling etc.) are debug noise; changes are worth keeping.
				lvl := slog.LevelInfo
				if r.Method == http.MethodGet {
					lvl = slog.LevelDebug
				}
				s.log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path,
					"status", sw.code, "ms", time.Since(start).Milliseconds())
			}
		}()
		if s.token != "" && r.URL.Path != "/healthz" && !s.authorized(r) {
			sw.Header().Set("WWW-Authenticate", `Bearer realm="subsvc"`)
			writeError(sw, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.token)) == 1
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func queryBool(r *http.Request, key string, def bool) (bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, errors.New("query parameter " + key + " must be true or false")
	}
	return b, nil
}

func validateLinkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("url must be a valid http:// or https:// address")
	}
	return nil
}

func storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "link not found")
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ------------------------------------------------------------------ status

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Status())
}

// ------------------------------------------------------------------- links

type linkRequest struct {
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Enabled *bool   `json:"enabled"`
}

func (s *Server) listLinks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Store().Links())
}

func (s *Server) getLink(w http.ResponseWriter, r *http.Request) {
	l, err := s.svc.Store().Link(r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) addLink(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.URL == nil {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	raw := strings.TrimSpace(*req.URL)
	if err := validateLinkURL(raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if len(name) > 100 {
		writeError(w, http.StatusBadRequest, "name must be at most 100 characters")
		return
	}
	if name == "" {
		u, _ := url.Parse(raw)
		name = u.Hostname()
	}
	l, err := s.svc.Store().AddLink(name, raw)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.log.Info("link added", "id", l.ID, "name", l.Name)
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) updateLink(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if !decodeBody(w, r, &req) {
		return
	}
	p := store.LinkPatch{Enabled: req.Enabled}
	if req.URL != nil {
		raw := strings.TrimSpace(*req.URL)
		if err := validateLinkURL(raw); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		p.URL = &raw
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 100 {
			writeError(w, http.StatusBadRequest, "name must be 1-100 characters")
			return
		}
		p.Name = &name
	}
	l, err := s.svc.Store().UpdateLink(r.PathValue("id"), p)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Store().DeleteLink(r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	s.log.Info("link deleted", "id", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// -------------------------------------------------------------------- jobs

func (s *Server) jobErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrBusy):
		writeError(w, http.StatusConflict, err.Error()+"; see GET /api/status or POST /api/cancel")
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, "link not found")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// POST /api/refresh?link=ID&test=true|false
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	test, err := queryBool(r, "test", true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.svc.StartRefresh(r.URL.Query().Get("link"), test)
	if err != nil {
		s.jobErr(w, err)
		return
	}
	s.log.Info("refresh started", "job", job.ID, "link", r.URL.Query().Get("link"), "test", test)
	writeJSON(w, http.StatusAccepted, job)
}

// POST /api/test?link=ID&only_untested=true|false
func (s *Server) test(w http.ResponseWriter, r *http.Request) {
	only, err := queryBool(r, "only_untested", false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.svc.StartTest(r.URL.Query().Get("link"), only)
	if err != nil {
		s.jobErr(w, err)
		return
	}
	s.log.Info("test started", "job", job.ID, "link", r.URL.Query().Get("link"), "only_untested", only)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if !s.svc.Cancel() {
		writeError(w, http.StatusConflict, "no job is running")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

// ----------------------------------------------------------------- configs

// GET /api/configs?reachable=true&link=ID
func (s *Server) configs(w http.ResponseWriter, r *http.Request) {
	reachable, err := queryBool(r, "reachable", false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	link := r.URL.Query().Get("link")
	if link != "" {
		if _, err := s.svc.Store().Link(link); err != nil {
			storeErr(w, err)
			return
		}
	}
	if reachable {
		writeJSON(w, http.StatusOK, s.svc.Store().Reachable(link))
		return
	}
	writeJSON(w, http.StatusOK, s.svc.Store().Entries(link))
}

// DELETE /api/configs/{key...}
// Delete a single config by its normalized key.
// Example: DELETE /api/configs/vless%3A%2F%2Fabc%40host%3A443%3F...
func (s *Server) deleteConfig(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	if err := s.svc.Store().DeleteEntry(key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("config deleted", "key", key)
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/configs?filter=failed|untested|unsupported|unreachable|all&link=ID
// Bulk delete.
//   - failed:      tested configs that are not reachable
//   - untested:    supported configs not yet tested
//   - unsupported: configs Xray cannot run
//   - unreachable: everything that is not reachable (covers untested + failed + unsupported)
//   - all:         every config (both healthy and broken)
func (s *Server) deleteConfigsBulk(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		writeError(w, http.StatusBadRequest,
			"filter is required (failed|untested|unsupported|unreachable|all)")
		return
	}
	link := r.URL.Query().Get("link")
	if link != "" {
		if _, err := s.svc.Store().Link(link); err != nil {
			storeErr(w, err)
			return
		}
	}
	n, err := s.svc.Store().DeleteEntries(filter, link)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("configs deleted", "filter", filter, "count", n, "link", link)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n,
		"filter":  filter,
		"link":    link,
	})
}

// GET /api/output?format=text|base64&link=ID&limit=N&name=NEW&numbered=true
//
// Only configs whose last real test succeeded, fastest first.
//
// Optional query params:
//   - name=STRING   : replace the display name (fragment / vmess "ps") of every
//                     emitted config with this string.
//   - numbered=true : append " 1", " 2", ... to the name so each line is unique.
//
// Example:
//
//	GET /api/output?format=base64&name=LONG%20LIVE%20IRAN&numbered=true
func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	link := q.Get("link")
	if link != "" {
		if _, err := s.svc.Store().Link(link); err != nil {
			storeErr(w, err)
			return
		}
	}
	format := q.Get("format")
	if format != "" && format != "text" && format != "base64" {
		writeError(w, http.StatusBadRequest, "format must be text or base64")
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}

	nameOverride := q.Get("name")
	numbered := q.Get("numbered") == "true"

	list := s.svc.Store().Reachable(link)
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}

	var b strings.Builder
	for i, e := range list {
		uri := e.URI
		if nameOverride != "" {
			final := nameOverride
			if numbered {
				final = nameOverride + " " + strconv.Itoa(i+1)
			}
			uri = renameURI(uri, final)
		}
		b.WriteString(uri)
		b.WriteByte('\n')
	}
	body := b.String()
	if format == "base64" {
		body = base64.StdEncoding.EncodeToString([]byte(body))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Config-Count", strconv.Itoa(len(list)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// ---------------------------------------------------------------- renaming

// renameURI replaces the display name of a proxy URI.
//
//   - vmess://  : the name is stored as "ps" inside the base64-encoded JSON payload
//   - everything else (vless, trojan, ss, ...): the name is the URI fragment after '#'
//
// If the URI cannot be parsed, it is returned unchanged.
func renameURI(uri, name string) string {
	if strings.HasPrefix(uri, "vmess://") {
		b64 := strings.TrimSpace(uri[len("vmess://"):])
		var data []byte
		var err error
		for _, dec := range []*base64.Encoding{
			base64.StdEncoding,
			base64.RawStdEncoding,
			base64.URLEncoding,
			base64.RawURLEncoding,
		} {
			data, err = dec.DecodeString(b64)
			if err == nil {
				break
			}
		}
		if err != nil {
			return uri
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			return uri
		}
		obj["ps"] = name
		newData, err := json.Marshal(obj)
		if err != nil {
			return uri
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(newData)
	}

	// All other schemes: strip existing fragment, append the new name.
	base := uri
	if i := strings.Index(uri, "#"); i >= 0 {
		base = uri[:i]
	}
	return base + "#" + url.PathEscape(name)
}