package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/poitch/mirage/internal/store"
)

func testAuth(t *testing.T) (*Authenticator, *store.DB, store.User) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ReconcileUsers(ctx, []store.UserMapping{
		{Username: "alice", DisplayName: "Alice", Home: "/homes/a"},
		{Username: "bob", DisplayName: "Bob", Home: "/homes/b"},
	}); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	alice, _ := db.UserByName(ctx, "alice")
	hash, _ := HashPassword("alice-account-password")
	if err := db.SetPasswordHash(ctx, alice.ID, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	alice, _ = db.UserByName(ctx, "alice")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewAuthenticator(db, log), db, alice
}

func TestVerifyAccountPassword(t *testing.T) {
	a, _, alice := testAuth(t)
	ctx := context.Background()

	got, err := a.Verify(ctx, "alice", "alice-account-password")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.ID != alice.ID {
		t.Errorf("got user %d, want %d", got.ID, alice.ID)
	}

	if _, err := a.Verify(ctx, "alice", "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong password: err = %v, want ErrUnauthorized", err)
	}
	if _, err := a.Verify(ctx, "nosuchuser", "whatever"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("unknown user: err = %v, want ErrUnauthorized", err)
	}
	if _, err := a.Verify(ctx, "alice", ""); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("empty secret: err = %v, want ErrUnauthorized", err)
	}
}

// TestVerifyAppPasswordIsBoundToItsOwner is the important one: a token is a
// credential for exactly one account. Accepting it under another username would
// let any user with a valid token impersonate anyone.
func TestVerifyAppPasswordIsBoundToItsOwner(t *testing.T) {
	a, db, alice := testAuth(t)
	ctx := context.Background()

	token, err := GenerateAppPassword()
	if err != nil {
		t.Fatalf("GenerateAppPassword: %v", err)
	}
	if _, err := db.CreateAppPassword(ctx, alice.ID, "test device", HashToken(token)); err != nil {
		t.Fatalf("CreateAppPassword: %v", err)
	}

	got, err := a.Verify(ctx, "alice", token)
	if err != nil {
		t.Fatalf("Verify with app password: %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("got %q, want alice", got.Username)
	}

	if _, err := a.Verify(ctx, "bob", token); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("alice's token under bob: err = %v, want ErrUnauthorized", err)
	}
}

// TestVerifyRejectsDisabledUser covers both credential paths, since a lock that
// only closed one of them would be no lock at all.
func TestVerifyRejectsDisabledUser(t *testing.T) {
	a, db, alice := testAuth(t)
	ctx := context.Background()

	token, _ := GenerateAppPassword()
	if _, err := db.CreateAppPassword(ctx, alice.ID, "device", HashToken(token)); err != nil {
		t.Fatalf("CreateAppPassword: %v", err)
	}
	if err := db.SetDisabled(ctx, alice.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	a.Forget("alice")

	if _, err := a.Verify(ctx, "alice", token); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("app password on disabled account: err = %v, want ErrUnauthorized", err)
	}
	if _, err := a.Verify(ctx, "alice", "alice-account-password"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("account password on disabled account: err = %v, want ErrUnauthorized", err)
	}
}

// TestVerifyRejectsUserWithNoPassword guards accounts that exist in config but
// have never had `mirage user passwd` run for them.
func TestVerifyRejectsUserWithNoPassword(t *testing.T) {
	a, _, _ := testAuth(t)
	for _, secret := range []string{"", "anything", "$argon2id$"} {
		if _, err := a.Verify(context.Background(), "bob", secret); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("secret %q against passwordless account: err = %v, want ErrUnauthorized", secret, err)
		}
	}
}

func TestGenerateAppPasswordIsRandomAndWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		tok, err := GenerateAppPassword()
		if err != nil {
			t.Fatalf("GenerateAppPassword: %v", err)
		}
		if len(tok) != appPasswordLen {
			t.Fatalf("length = %d, want %d", len(tok), appPasswordLen)
		}
		for _, c := range tok {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				t.Fatalf("token contains non-alphanumeric %q", c)
			}
		}
		if seen[tok] {
			t.Fatal("duplicate token generated")
		}
		seen[tok] = true
	}
}
