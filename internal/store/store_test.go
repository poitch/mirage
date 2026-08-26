package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "mirage.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "dir", "mirage.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := db.ReconcileUsers(ctx, []UserMapping{{Username: "alice", Home: "/h/a"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()

	// Re-opening must migrate cleanly and preserve existing rows.
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	if _, err := db2.UserByName(ctx, "alice"); err != nil {
		t.Fatalf("user lost across reopen: %v", err)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db := testDB(t)
	// A dangling user_id must be rejected. This only holds if the foreign_keys
	// pragma reached this pooled connection.
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO app_passwords (user_id, token_hash, created_at) VALUES (?, ?, ?)`,
		9999, []byte("x"), 0)
	if err == nil {
		t.Fatal("expected foreign key violation, insert succeeded")
	}
}

func TestReconcileUsers(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	res, err := db.ReconcileUsers(ctx, []UserMapping{
		{Username: "alice", DisplayName: "Alice", Home: "/homes/a", UID: 1026, GID: 100},
		{Username: "bob", DisplayName: "Bob", Home: "/homes/b", UID: 1027, GID: 100},
	})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("Created = %v, want 2 users", res.Created)
	}

	alice, err := db.UserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}
	if alice.UID != 1026 || alice.GID != 100 {
		t.Errorf("alice uid/gid = %d/%d, want 1026/100", alice.UID, alice.GID)
	}
	if err := db.SetPasswordHash(ctx, alice.ID, "hash-v1"); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}

	// Re-running with no changes must be a no-op, not a churn of updates.
	res, err = db.ReconcileUsers(ctx, []UserMapping{
		{Username: "alice", DisplayName: "Alice", Home: "/homes/a", UID: 1026, GID: 100},
		{Username: "bob", DisplayName: "Bob", Home: "/homes/b", UID: 1027, GID: 100},
	})
	if err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}
	if len(res.Created)+len(res.Updated)+len(res.Disabled) != 0 {
		t.Errorf("re-reconcile changed something: %+v", res)
	}

	// Changing the mapping must update it but leave the password alone.
	res, err = db.ReconcileUsers(ctx, []UserMapping{
		{Username: "alice", DisplayName: "Alice", Home: "/homes/a", UID: 2000, GID: 100},
		{Username: "bob", DisplayName: "Bob", Home: "/homes/b", UID: 1027, GID: 100},
	})
	if err != nil {
		t.Fatalf("uid change: %v", err)
	}
	if len(res.Updated) != 1 || res.Updated[0] != "alice" {
		t.Errorf("Updated = %v, want [alice]", res.Updated)
	}
	alice, _ = db.UserByName(ctx, "alice")
	if alice.UID != 2000 {
		t.Errorf("alice uid = %d, want 2000", alice.UID)
	}
	if alice.PasswordHash != "hash-v1" {
		t.Errorf("password hash = %q, config reconcile must not touch credentials", alice.PasswordHash)
	}

	// Dropping a user from config disables rather than deletes, so their
	// credentials and history survive a config typo.
	res, err = db.ReconcileUsers(ctx, []UserMapping{
		{Username: "alice", DisplayName: "Alice", Home: "/homes/a", UID: 2000, GID: 100},
	})
	if err != nil {
		t.Fatalf("drop bob: %v", err)
	}
	if len(res.Disabled) != 1 || res.Disabled[0] != "bob" {
		t.Errorf("Disabled = %v, want [bob]", res.Disabled)
	}
	bob, err := db.UserByName(ctx, "bob")
	if err != nil {
		t.Fatalf("bob should still exist: %v", err)
	}
	if !bob.Disabled {
		t.Error("bob should be disabled")
	}

	// Restoring them to config re-enables in place.
	if _, err := db.ReconcileUsers(ctx, []UserMapping{
		{Username: "alice", DisplayName: "Alice", Home: "/homes/a", UID: 2000, GID: 100},
		{Username: "bob", DisplayName: "Bob", Home: "/homes/b", UID: 1027, GID: 100},
	}); err != nil {
		t.Fatalf("restore bob: %v", err)
	}
	bob, _ = db.UserByName(ctx, "bob")
	if bob.Disabled {
		t.Error("bob should be re-enabled after returning to config")
	}
}

// TestReconcileMovedHomeDropsIndex covers the correctness trap in a moved home:
// the cached file IDs and ETags describe the old directory, so they must be
// discarded rather than served against the new one.
func TestReconcileMovedHomeDropsIndex(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	if _, err := db.ReconcileUsers(ctx, []UserMapping{
		{Username: "alice", Home: "/homes/a", UID: 1026, GID: 100},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	alice, _ := db.UserByName(ctx, "alice")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO nodes (user_id, path, name, is_dir, etag) VALUES (?, 'docs/x.txt', 'x.txt', 0, 'e1')`,
		alice.ID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	res, err := db.ReconcileUsers(ctx, []UserMapping{
		{Username: "alice", Home: "/homes/a-moved", UID: 1026, GID: 100},
	})
	if err != nil {
		t.Fatalf("move home: %v", err)
	}
	if len(res.Reindex) != 1 || res.Reindex[0] != "alice" {
		t.Fatalf("Reindex = %v, want [alice]", res.Reindex)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE user_id = ?`, alice.ID).Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if n != 0 {
		t.Errorf("index has %d stale nodes after home move, want 0", n)
	}
}

func TestUserNotFound(t *testing.T) {
	_, err := testDB(t).UserByName(context.Background(), "nobody")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestAdminLockSurvivesReconcile covers the security-relevant case: locking a
// compromised account must not be silently undone by the next config reload,
// even though the account is still listed in the config file.
func TestAdminLockSurvivesReconcile(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	mappings := []UserMapping{{Username: "alice", Home: "/homes/a", UID: 1026, GID: 100}}
	if _, err := db.ReconcileUsers(ctx, mappings); err != nil {
		t.Fatalf("seed: %v", err)
	}
	alice, _ := db.UserByName(ctx, "alice")

	if err := db.SetDisabled(ctx, alice.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}

	res, err := db.ReconcileUsers(ctx, mappings)
	if err != nil {
		t.Fatalf("reconcile after lock: %v", err)
	}
	if len(res.Updated) != 0 {
		t.Errorf("Updated = %v, want no change for an admin-locked user", res.Updated)
	}

	alice, _ = db.UserByName(ctx, "alice")
	if !alice.Disabled {
		t.Fatal("admin lock was cleared by config reconcile")
	}
	if alice.DisabledReason != ReasonAdmin {
		t.Errorf("DisabledReason = %q, want %q", alice.DisabledReason, ReasonAdmin)
	}

	// An explicit enable is the only thing that lifts it.
	if err := db.SetDisabled(ctx, alice.ID, false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	alice, _ = db.UserByName(ctx, "alice")
	if alice.Disabled || alice.DisabledReason != "" {
		t.Errorf("after enable: disabled=%v reason=%q, want enabled", alice.Disabled, alice.DisabledReason)
	}
}

// TestConfigDisableIsLifted is the counterpart: a user that merely fell out of
// the config comes back automatically when it is restored.
func TestConfigDisableIsLifted(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	full := []UserMapping{
		{Username: "alice", Home: "/homes/a"},
		{Username: "bob", Home: "/homes/b"},
	}
	if _, err := db.ReconcileUsers(ctx, full); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.ReconcileUsers(ctx, full[:1]); err != nil {
		t.Fatalf("drop bob: %v", err)
	}
	bob, _ := db.UserByName(ctx, "bob")
	if !bob.Disabled || bob.DisabledReason != ReasonConfig {
		t.Fatalf("bob: disabled=%v reason=%q, want config-disabled", bob.Disabled, bob.DisabledReason)
	}

	if _, err := db.ReconcileUsers(ctx, full); err != nil {
		t.Fatalf("restore bob: %v", err)
	}
	bob, _ = db.UserByName(ctx, "bob")
	if bob.Disabled || bob.DisabledReason != "" {
		t.Errorf("bob: disabled=%v reason=%q, want re-enabled", bob.Disabled, bob.DisabledReason)
	}
}
