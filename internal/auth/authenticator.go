package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/poitch/mirage/internal/store"
)

// ErrUnauthorized is returned for any failed credential check. It is
// deliberately the single error for every cause, so a caller cannot leak
// whether an account exists by branching on it.
var ErrUnauthorized = errors.New("invalid credentials")

const (
	// credentialCacheSize bounds memory; each entry is a hash and an ID.
	credentialCacheSize = 2048
	// credentialCacheTTL keeps a verified account password usable without
	// re-running argon2 on every request, while bounding how long a changed or
	// revoked password stays accepted.
	credentialCacheTTL = 60 * time.Second
)

// Authenticator verifies client credentials against the store.
type Authenticator struct {
	db    *store.DB
	log   *slog.Logger
	cache *expirable.LRU[string, int64]
}

// NewAuthenticator builds an Authenticator.
func NewAuthenticator(db *store.DB, log *slog.Logger) *Authenticator {
	return &Authenticator{
		db:    db,
		log:   log,
		cache: expirable.NewLRU[string, int64](credentialCacheSize, nil, credentialCacheTTL),
	}
}

// Verify checks a username and secret, returning the authenticated user.
//
// The secret may be either an app password, which is what sync clients present
// on every request, or the account password used during interactive login. App
// passwords are tried first because they are both the common case and the cheap
// one.
func (a *Authenticator) Verify(ctx context.Context, username, secret string) (store.User, error) {
	if username == "" || secret == "" {
		return store.User{}, ErrUnauthorized
	}

	cacheKey := credentialKey(username, secret)
	if userID, ok := a.cache.Get(cacheKey); ok {
		user, err := a.db.UserByID(ctx, userID)
		if err == nil && !user.Disabled && user.Username == username {
			return user, nil
		}
		// The account changed underneath the cache entry; drop it and fall
		// through to a full check.
		a.cache.Remove(cacheKey)
	}

	tokenHash := HashToken(secret)
	if user, err := a.db.UserByAppPassword(ctx, tokenHash); err == nil {
		// A token is bound to one account. Presenting a valid token under
		// another user's name must not authenticate either of them.
		if subtle.ConstantTimeCompare([]byte(user.Username), []byte(username)) != 1 {
			return store.User{}, ErrUnauthorized
		}
		if err := a.db.TouchAppPassword(ctx, tokenHash); err != nil {
			a.log.Debug("could not record app password use", "error", err)
		}
		a.cache.Add(cacheKey, user.ID)
		return user, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}

	user, err := a.db.UserByName(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrUnauthorized
		}
		return store.User{}, err
	}
	if user.Disabled {
		return store.User{}, ErrUnauthorized
	}
	// VerifyPassword returns false for an empty hash, so an account with no
	// password set cannot be logged into with an empty secret.
	if !VerifyPassword(user.PasswordHash, secret) {
		return store.User{}, ErrUnauthorized
	}

	a.cache.Add(cacheKey, user.ID)
	return user, nil
}

// Forget drops any cached verification for a user. Callers use it after
// changing or revoking credentials so the change takes effect at once rather
// than after the cache TTL.
func (a *Authenticator) Forget(username string) {
	prefix := username + "\x00"
	for _, k := range a.cache.Keys() {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			a.cache.Remove(k)
		}
	}
}

// credentialKey derives a cache key. The secret is hashed so that a memory dump
// does not hand over usable credentials, and the username is kept in the clear
// prefix so Forget can find a user's entries.
func credentialKey(username, secret string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + secret))
	return username + "\x00" + hex.EncodeToString(sum[:])
}
