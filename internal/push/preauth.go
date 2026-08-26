package push

import (
	"crypto/rand"
	"net/http"
	"sync"
	"time"

	"github.com/poitch/mirage/internal/auth"
)

// preAuthTTL is how long a pre-auth token stays usable.
//
// It is deliberately short. The token is fetched over an authenticated request
// and handed straight to a websocket connection, so it only has to survive that
// round trip.
const preAuthTTL = 2 * time.Minute

// tokenStore holds single-use pre-auth tokens in memory.
//
// They are not persisted: a restart drops every push connection anyway, and a
// client simply asks for another token when it reconnects.
type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]tokenEntry
}

type tokenEntry struct {
	userID  int64
	expires time.Time
}

func newTokenStore() *tokenStore {
	return &tokenStore{tokens: make(map[string]tokenEntry)}
}

// issue mints a token for a user.
func (s *tokenStore) issue(userID int64) string {
	token := rand.Text()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Expired entries are cleared here rather than on a timer: the map only
	// grows when tokens are issued, so that is exactly when it needs tidying.
	now := time.Now()
	for t, e := range s.tokens {
		if now.After(e.expires) {
			delete(s.tokens, t)
		}
	}
	s.tokens[token] = tokenEntry{userID: userID, expires: now.Add(preAuthTTL)}
	return token
}

// consume redeems a token, which may only be used once.
func (s *tokenStore) consume(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tokens[token]
	if !ok {
		return 0, false
	}
	delete(s.tokens, token)
	if time.Now().After(entry.expires) {
		return 0, false
	}
	return entry.userID, true
}

// PreAuth issues a token for the authenticated user, for a client that can make
// authenticated requests but would rather not put its password on the websocket.
//
// The response is the bare token as text, which is what clients expect.
func (h *Hub) PreAuth(w http.ResponseWriter, r *http.Request) {
	user := auth.MustUser(r.Context())
	token := h.preAuth.issue(user.ID)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	//nolint:errcheck // connection gone
	w.Write([]byte(token))
}
