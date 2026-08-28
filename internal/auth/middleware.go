package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/poitch/mirage/internal/store"
)

type ctxKey struct{}

// WithUser returns a copy of ctx carrying the authenticated user.
func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom retrieves the authenticated user from ctx.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(store.User)
	return u, ok
}

// MustUser retrieves the authenticated user, panicking if there is none.
// Handlers behind Require may use it, since reaching them without a user would
// be a routing bug rather than a request the caller controls.
func MustUser(ctx context.Context) store.User {
	u, ok := UserFrom(ctx)
	if !ok {
		panic("auth: no authenticated user in context; handler is not behind Require")
	}
	return u
}

// Require wraps next so that only authenticated requests reach it.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, secret, ok := r.BasicAuth()
		if !ok {
			a.challenge(w, r, "missing credentials")
			return
		}

		user, err := a.Verify(r.Context(), username, secret)
		if err != nil {
			if !errors.Is(err, ErrUnauthorized) {
				// A client hanging up mid-request cancels the context and the
				// credential lookup fails with it. That is the client's doing,
				// not a fault here, and reporting it as an error buried the
				// real ones. There is nobody left to answer either.
				if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
					a.log.Debug("client disconnected before authentication finished",
						"user", username, "path", r.URL.Path)
					return
				}
				a.log.Error("authentication failed", "error", err, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			a.log.Info("rejected credentials",
				"user", username, "path", r.URL.Path, "agent", r.UserAgent())
			a.challenge(w, r, "invalid credentials")
			return
		}

		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), user)))
	})
}

// challenge writes a 401.
//
// The WWW-Authenticate header is what tells a sync client to retry with
// credentials, so it must be present. It is suppressed for browsers, where it
// would raise a native password box that has nothing to do with the pairing
// flow the user is actually in.
func (a *Authenticator) challenge(w http.ResponseWriter, r *http.Request, reason string) {
	if !isBrowser(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Mirage", charset="UTF-8"`)
	}
	http.Error(w, reason, http.StatusUnauthorized)
}

// isBrowser reports whether the request looks like a top-level browser
// navigation rather than an API client.
func isBrowser(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Mode") == "navigate"
}
