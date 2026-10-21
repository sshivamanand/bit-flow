package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testHost = "127.0.0.1:7710"

func newTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("BITFLOW_CONFIG_DIR", t.TempDir())
	app, err := NewApp("test")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{app: app, token: "secret-token", host: testHost}
}

func do(t *testing.T, s *Server, method, path, body string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://"+testHost+path, strings.NewReader(body))
	r.Host = testHost
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func TestAPIRequiresAuth(t *testing.T) {
	s := newTestServer(t)
	if w := do(t, s, "GET", "/api/state", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: got %d", w.Code)
	}
	if w := do(t, s, "GET", "/api/state", "", map[string]string{"X-Bitflow-Token": "secret-token"}); w.Code != http.StatusOK {
		t.Fatalf("token: got %d", w.Code)
	}
	if w := do(t, s, "GET", "/api/state", "", map[string]string{"X-Bitflow-Token": "wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d", w.Code)
	}
}

func TestHostAndCSRFGuards(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest("GET", "/api/state", nil)
	r.Host = "evil.example"
	r.Header.Set("X-Bitflow-Token", "secret-token")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign Host: got %d", w.Code)
	}
	// Writes need the custom header a cross-site form can't send.
	if w := do(t, s, "POST", "/api/scan", "{}", map[string]string{"X-Bitflow-Token": "secret-token"}); w.Code != http.StatusForbidden {
		t.Fatalf("POST without X-Bitflow: got %d", w.Code)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newTestServer(t)
	hdr := map[string]string{"X-Bitflow": "1", "X-Bitflow-Token": "secret-token"}

	w := do(t, s, "POST", "/api/session/start", "", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("bad session cookie: %+v", cookie)
	}

	// The cookie alone now authenticates, and a new session must pick a name.
	var view sessionView
	w = do(t, s, "GET", "/api/session", "", nil, cookie)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil || !view.NeedsName {
		t.Fatalf("session: %d %s", w.Code, w.Body)
	}
	w = do(t, s, "POST", "/api/session", `{"name":"  gaming rig ","theme":"violet"}`, map[string]string{"X-Bitflow": "1"}, cookie)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.NeedsName || view.Name != "gaming rig" || view.Theme != "violet" {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if got := s.app.DeviceName(); got != "gaming rig" {
		t.Fatalf("device name = %q", got)
	}
	if w := do(t, s, "POST", "/api/session", `{"theme":"neon"}`, map[string]string{"X-Bitflow": "1"}, cookie); w.Code == http.StatusOK {
		t.Fatal("unknown theme accepted")
	}

	// Sessions survive a restart, stored by hash only.
	dir, _ := filepath.Abs(os.Getenv("BITFLOW_CONFIG_DIR"))
	raw, _ := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if strings.Contains(string(raw), cookie.Value) {
		t.Fatal("raw session ID written to disk")
	}
	if _, ok := loadSessions(dir).get(cookie.Value); !ok {
		t.Fatal("session not persisted")
	}

	// Signing out invalidates the cookie.
	if w := do(t, s, "POST", "/api/session/forget", "{}", map[string]string{"X-Bitflow": "1"}, cookie); w.Code != http.StatusOK {
		t.Fatalf("forget: %d", w.Code)
	}
	if w := do(t, s, "GET", "/api/state", "", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("after forget: %d", w.Code)
	}
}

func TestStartSessionNeedsToken(t *testing.T) {
	s := newTestServer(t)
	if w := do(t, s, "POST", "/api/session/start", "", map[string]string{"X-Bitflow": "1"}); w.Code != http.StatusForbidden {
		t.Fatalf("got %d", w.Code)
	}
}

func TestIndexServed(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "bit-flow") {
		t.Fatalf("index: %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("missing CSP")
	}
}

func TestDocsPage(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/docs", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "send something") {
		t.Fatalf("docs: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/docs", nil)
	r.Host = "evil.example"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign host got %d", rec.Code)
	}
}
