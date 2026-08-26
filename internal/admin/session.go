// Package admin serves the account management page.
//
// It exists so that adding a user does not mean editing a YAML file on the NAS
// and restarting the container. Accounts live in the database; the config file
// may still declare them, in which case it wins and this page is read-only.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie = "mirage_admin"
	// sessionTTL bounds an idle session. Short, because this page can change
	// where every account's files live.
	sessionTTL = 8 * time.Hour
	// sessionIdle expires a session that has gone quiet, independently of TTL.
	sessionIdle = 60 * time.Minute
)

type session struct {
	csrf     string
	created  time.Time
	lastSeen time.Time
}

// sessionStore holds admin sessions in memory.
//
// Deliberately not persisted: a restart logging every admin out is the correct
// behaviour for a session that grants this much, and it keeps session material
// off disk.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*session)}
}

// create starts a session and returns its token.
func (s *sessionStore) create() (token, csrf string) {
	token = rand.Text() + rand.Text()
	csrf = rand.Text()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	now := time.Now()
	s.sessions[token] = &session{csrf: csrf, created: now, lastSeen: now}
	return token, csrf
}

// lookup returns a live session, refreshing its idle timer.
func (s *sessionStore) lookup(token string) (*session, bool) {
	if token == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[token]
	if !ok {
		return nil, false
	}
	now := time.Now()
	if now.Sub(sess.created) > sessionTTL || now.Sub(sess.lastSeen) > sessionIdle {
		delete(s.sessions, token)
		return nil, false
	}
	sess.lastSeen = now
	return sess, true
}

func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// destroyAll ends every session. Used when credentials change underneath them.
func (s *sessionStore) destroyAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]*session)
}

func (s *sessionStore) pruneLocked() {
	now := time.Now()
	for token, sess := range s.sessions {
		if now.Sub(sess.created) > sessionTTL || now.Sub(sess.lastSeen) > sessionIdle {
			delete(s.sessions, token)
		}
	}
}

// validCSRF reports whether a submitted token matches the session's.
func (s *session) validCSRF(submitted string) bool {
	return subtle.ConstantTimeCompare([]byte(s.csrf), []byte(submitted)) == 1
}

// setSessionCookie writes the session cookie.
//
// Secure is set only when the server is reached over https, because a Secure
// cookie is simply dropped over plain http and the admin would be unable to log
// in at all on a LAN-only setup.
func setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
