// Package web serves a small browser view of an account's files.
//
// Mirage is a sync server and this is deliberately not a file manager: it
// browses, and it downloads. It exists because a search result has to lead
// somewhere. The desktop client resolves most of them locally and never comes
// here; the ones it cannot, and any link somebody pastes to another person,
// land on these pages.
//
// Everything here is scoped to the signed-in account through the same confined
// storage the sync endpoints use, so a browser session can reach exactly what a
// sync client for that account can reach and nothing else.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie = "mirage_session"
	// sessionTTL bounds a session outright, and sessionIdle expires one that
	// has gone quiet. Longer than the admin page's, because this grants only
	// what the person's own sync client already has.
	sessionTTL  = 30 * 24 * time.Hour
	sessionIdle = 14 * 24 * time.Hour
)

type session struct {
	userID   int64
	username string
	csrf     string
	created  time.Time
	lastSeen time.Time
}

// sessionStore holds browser sessions in memory.
//
// Not persisted: a restart signing everyone out of the web view costs a login,
// while writing session material to disk costs rather more if the database is
// ever copied off the NAS.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*session)}
}

func (s *sessionStore) create(userID int64, username string) (token, csrf string) {
	token = rand.Text() + rand.Text()
	csrf = rand.Text()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	now := time.Now()
	s.sessions[token] = &session{
		userID: userID, username: username, csrf: csrf, created: now, lastSeen: now,
	}
	return token, csrf
}

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

// destroyFor ends every session belonging to an account, for when its password
// changes or it is disabled underneath them.
func (s *sessionStore) destroyFor(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, sess := range s.sessions {
		if sess.userID == userID {
			delete(s.sessions, token)
		}
	}
}

func (s *sessionStore) pruneLocked() {
	now := time.Now()
	for token, sess := range s.sessions {
		if now.Sub(sess.created) > sessionTTL || now.Sub(sess.lastSeen) > sessionIdle {
			delete(s.sessions, token)
		}
	}
}

func (s *session) validCSRF(submitted string) bool {
	return subtle.ConstantTimeCompare([]byte(s.csrf), []byte(submitted)) == 1
}

func setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
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
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
