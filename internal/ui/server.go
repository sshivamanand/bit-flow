package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sshivamanand/bit-flow/internal/steam"
)

//go:embed static/index.html static/docs.html
var static embed.FS

// Server serves the UI on a loopback address. API calls need a session
// cookie (obtained once by trading the launch URL's token) or the token itself;
// the Host header must be the loopback address (no DNS rebinding), and writes
// need a custom header that cross-site pages can't send.
type Server struct {
	app   *App
	token string
	host  string // "127.0.0.1:port"
}

// Serve starts the UI server on addr (e.g. "127.0.0.1:7710", falling back to
// a free port if that's taken) and returns the URL to open.
func Serve(ctx context.Context, app *App, addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		host, _, _ := net.SplitHostPort(addr)
		if ln, err = net.Listen("tcp", net.JoinHostPort(host, "0")); err != nil {
			return "", err
		}
	}
	var tok [16]byte
	_, _ = rand.Read(tok[:])
	s := &Server{app: app, token: hex.EncodeToString(tok[:]), host: ln.Addr().String()}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go srv.Serve(ln)
	return fmt.Sprintf("http://%s/#t=%s", s.host, s.token), nil
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error { return openURL(url) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /docs", s.docs)
	mux.HandleFunc("POST /api/session/start", s.startSession)
	mux.HandleFunc("GET /api/session", s.auth(s.getSession))
	mux.HandleFunc("POST /api/session", s.auth(s.updateSession))
	mux.HandleFunc("POST /api/session/forget", s.auth(s.forgetSession))
	mux.HandleFunc("GET /api/state", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.app.State())
	}))
	mux.HandleFunc("GET /api/events", s.auth(s.events))
	mux.HandleFunc("POST /api/scan", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.app.Scan()
		writeJSON(w, struct{}{})
	}))
	mux.HandleFunc("POST /api/pick", s.auth(s.pick))
	mux.HandleFunc("POST /api/send", s.auth(s.send))
	mux.HandleFunc("GET /api/games", s.auth(func(w http.ResponseWriter, r *http.Request) {
		games := s.app.Games()
		if games == nil {
			games = []steam.Game{}
		}
		writeJSON(w, games)
	}))
	mux.HandleFunc("POST /api/offers/{id}", s.auth(s.decide))
	mux.HandleFunc("POST /api/transfers/{id}/cancel", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, s.app.Cancel(r.PathValue("id")))
	}))
	mux.HandleFunc("POST /api/transfers/clear", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.app.ClearFinished()
		writeJSON(w, struct{}{})
	}))
	mux.HandleFunc("POST /api/settings", s.auth(s.settings))
	mux.HandleFunc("POST /api/reveal", s.auth(s.reveal))
	return mux
}

// sameHost blocks DNS rebinding: the browser must be talking to the loopback address.
func (s *Server) sameHost(r *http.Request) bool {
	if r.Host == s.host {
		return true
	}
	_, port, _ := net.SplitHostPort(s.host)
	return r.Host == "localhost:"+port
}

// validToken reports whether r carries the launch token.
func (s *Server) validToken(r *http.Request) bool {
	tok := r.Header.Get("X-Bitflow-Token")
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

func sessionID(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameHost(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Cross-site pages can't add custom headers without a CORS preflight,
		// which this server never grants.
		if r.Method != http.MethodGet && r.Header.Get("X-Bitflow") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !s.validToken(r) {
			if _, ok := s.app.sessions.get(sessionID(r)); !ok {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]string{"error": "no session: open bit-flow from the link it printed"})
				return
			}
		}
		h(w, r)
	}
}

// startSession trades the launch token for a long-lived session cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	if !s.sameHost(r) || r.Header.Get("X-Bitflow") != "1" || !s.validToken(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if sess, ok := s.app.sessions.get(sessionID(r)); ok {
		writeJSON(w, s.sessionView(sess))
		return
	}
	id, sess := s.app.sessions.create(r.UserAgent(), "")
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", MaxAge: int(sessionMaxAge.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, s.sessionView(*sess))
}

type sessionView struct {
	Name      string    `json:"name"`
	Theme     string    `json:"theme"`
	Suggested string    `json:"suggested"` // current device name, to prefill onboarding
	Created   time.Time `json:"created"`
	NeedsName bool      `json:"needs_name"`
}

func (s *Server) sessionView(sess Session) sessionView {
	return sessionView{Name: sess.Name, Theme: sess.Theme, Suggested: s.app.DeviceName(),
		Created: sess.Created, NeedsName: sess.Name == ""}
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.app.sessions.get(sessionID(r))
	if !ok {
		// Token-authenticated callers (scripts) have no session of their own.
		writeJSON(w, s.sessionView(Session{Name: s.app.DeviceName(), Theme: "flow"}))
		return
	}
	writeJSON(w, s.sessionView(sess))
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  *string `json:"name"`
		Theme *string `json:"theme"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 40 {
			reply(w, nil, errors.New("pick a name between 1 and 40 characters"))
			return
		}
		if err := s.app.SetDeviceName(name); err != nil {
			reply(w, nil, err)
			return
		}
		req.Name = &name
	}
	if req.Theme != nil && !themes[*req.Theme] {
		reply(w, nil, errors.New("unknown theme"))
		return
	}
	sess, ok := s.app.sessions.update(sessionID(r), func(sess *Session) {
		if req.Name != nil {
			sess.Name = *req.Name
		}
		if req.Theme != nil {
			sess.Theme = *req.Theme
		}
	})
	if !ok {
		writeJSON(w, struct{}{})
		return
	}
	writeJSON(w, s.sessionView(sess))
}

func (s *Server) forgetSession(w http.ResponseWriter, r *http.Request) {
	s.app.sessions.delete(sessionID(r))
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, struct{}{})
}

// docs serves the built-in quick reference. It's static text, so it needs no login.
func (s *Server) docs(w http.ResponseWriter, r *http.Request) {
	if !s.sameHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	b, _ := static.ReadFile("static/docs.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	_, _ = w.Write(b)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if !s.sameHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	b, _ := static.ReadFile("static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:")
	_, _ = w.Write(b)
}

// events streams state snapshots whenever they change (Server-Sent Events).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	var last []byte
	lastSent := time.Time{}
	for {
		b, _ := json.Marshal(s.app.State())
		if !bytes.Equal(b, last) || time.Since(lastSent) > 15*time.Second {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
			last, lastSent = b, time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) pick(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	paths, err := pick(req.Kind)
	if errors.Is(err, errCanceled) {
		writeJSON(w, map[string]any{"paths": []string{}})
		return
	}
	reply(w, map[string]any{"paths": paths}, err)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To    string   `json:"to"`
		Peer  string   `json:"peer"`
		Paths []string `json:"paths"`
		Code  string   `json:"code"`
		Steam string   `json:"steam"` // app ID: send this Steam game instead of paths
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Steam != "" {
		id, err := s.app.SendSteam(req.To, req.Peer, req.Steam, req.Code)
		reply(w, map[string]string{"id": id}, err)
		return
	}
	id, err := s.app.Send(req.To, req.Peer, req.Paths, req.Code, false)
	reply(w, map[string]string{"id": id}, err)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Accept bool   `json:"accept"`
		Dest   string `json:"dest"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	reply(w, nil, s.app.Decide(r.PathValue("id"), req.Accept, req.Dest))
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	var req Settings
	if !readJSON(w, r, &req) {
		return
	}
	reply(w, nil, s.app.UpdateSettings(req))
}

func (s *Server) reveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	reply(w, nil, reveal(req.Path))
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "bad request: " + err.Error()})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if v == nil {
		v = struct{}{}
	}
	writeJSON(w, v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
