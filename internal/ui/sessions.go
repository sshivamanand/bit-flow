package ui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Browser sessions. The launch URL carries a one-time token; the page trades
// it for a session cookie (HttpOnly, SameSite=Strict) and drops the token from
// the address bar. Sessions are saved in the config directory, so reopening
// the app later, even after a restart, needs no token. Only a hash of each
// session ID is stored, so the file alone can't be replayed as a cookie.

const (
	sessionCookie = "bitflow_session"
	sessionMaxAge = 365 * 24 * time.Hour
)

// Themes the UI offers.
var themes = map[string]bool{"flow": true, "ember": true, "violet": true, "paper": true}

// Session is one browser's identity and preferences.
type Session struct {
	Name     string    `json:"name"`
	Theme    string    `json:"theme"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	Agent    string    `json:"agent,omitempty"`
}

type sessionStore struct {
	path string
	mu   sync.Mutex
	m    map[string]*Session // keyed by sha256(session id)
}

func loadSessions(dir string) *sessionStore {
	s := &sessionStore{path: filepath.Join(dir, "sessions.json"), m: map[string]*Session{}}
	b, err := os.ReadFile(s.path)
	if err == nil {
		_ = json.Unmarshal(b, &s.m)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return s
	}
	// Drop sessions nobody has used for a year.
	for k, v := range s.m {
		if time.Since(v.LastSeen) > sessionMaxAge {
			delete(s.m, k)
		}
	}
	return s
}

func hashID(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// create starts a session and returns its secret ID (the cookie value).
func (s *sessionStore) create(agent, name string) (string, *Session) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	now := time.Now()
	sess := &Session{Name: name, Theme: "flow", Created: now, LastSeen: now, Agent: agent}
	s.mu.Lock()
	s.m[hashID(id)] = sess
	s.saveLocked()
	s.mu.Unlock()
	return id, sess
}

// get returns a copy of the session for id, refreshing its last-seen time.
func (s *sessionStore) get(id string) (Session, bool) {
	if id == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[hashID(id)]
	if !ok {
		return Session{}, false
	}
	if time.Since(sess.LastSeen) > time.Hour {
		sess.LastSeen = time.Now()
		s.saveLocked()
	}
	return *sess, true
}

func (s *sessionStore) update(id string, f func(*Session)) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[hashID(id)]
	if !ok {
		return Session{}, false
	}
	f(sess)
	sess.LastSeen = time.Now()
	s.saveLocked()
	return *sess, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, hashID(id))
	s.saveLocked()
}

func (s *sessionStore) saveLocked() {
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}
