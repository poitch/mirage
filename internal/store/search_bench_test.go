package store

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// benchWords build names that look like the ones people actually have, so that
// a search term matches a plausible fraction of them.
var benchWords = []string{
	"report", "invoice", "holiday", "photo", "scan", "backup", "notes", "draft",
	"final", "budget", "receipt", "letter", "contract", "summer", "winter",
	"family", "wedding", "screenshot", "recipe", "tax", "insurance", "school",
}

// buildBenchIndex fills an index with n files. It is the slow part of the
// benchmark, so it is built once and reused.
func buildBenchIndex(b *testing.B, n int) (*DB, User) {
	b.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(b.TempDir(), "index.db"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { db.Close() })

	if _, err := db.ReconcileUsers(ctx, []UserMapping{{Username: "alice", Home: "/tmp/alice"}}); err != nil {
		b.Fatalf("seed user: %v", err)
	}
	u, _ := db.UserByName(ctx, "alice")

	rng := rand.New(rand.NewSource(1))
	tx, err := db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(
		`INSERT INTO nodes (id, user_id, path, name, is_dir, size, etag, content_type)
		 VALUES (?, ?, ?, ?, 0, 1, ?, '')`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("%s-%s-%04d.pdf",
			benchWords[rng.Intn(len(benchWords))], benchWords[rng.Intn(len(benchWords))], rng.Intn(10000))
		if _, err := stmt.Exec(i, u.ID, fmt.Sprintf("d/%d/%s", i, name), name,
			fmt.Sprintf("%016x", rng.Int63())); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}

	// The one name nothing else shares: a search that matches almost nothing is
	// the case an unindexed scan is worst at, because it cannot stop early.
	if _, err := UpsertNode(ctx, db, Node{
		UserID: u.ID, Path: "rare/aardvark-lease.pdf", Name: "aardvark-lease.pdf",
	}, Stamp()); err != nil {
		b.Fatalf("insert rare: %v", err)
	}
	return db, u
}

func BenchmarkSearchNodes(b *testing.B) {
	const rows = 1_000_000
	db, u := buildBenchIndex(b, rows)
	ctx := context.Background()

	for _, tc := range []struct{ name, pattern string }{
		{"rare", "%aardvark%"},
		{"common", "%wedding%"},
		// Too short to have a trigram, so this falls back to reading every row
		// in the account. It doubles as the baseline: it is what every search
		// cost before the index existed.
		{"short-term-fallback", "%cv%"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := SearchNodes(ctx, db, u.ID, ".", tc.pattern, "", 100); err != nil {
					b.Fatalf("SearchNodes: %v", err)
				}
			}
		})
	}
}
