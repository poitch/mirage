package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiteralRuns(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{"%report%", []string{"report"}},
		{"report", []string{"report"}},
		{"%a%b%", []string{"a", "b"}},
		{"%holiday%2024%", []string{"holiday", "2024"}},
		{"%", nil},
		{"", nil},
		{"%budget_v2%", []string{"budget", "v2"}},
		// An escaped wildcard is ordinary text and belongs to the run.
		{`%50\%off%`, []string{"50%off"}},
		{`%a\_b%`, []string{"a_b"}},
		// Malformed, but the LIKE decides the answer either way.
		{`%abc\`, []string{`abc\`}},
	} {
		got := literalRuns(tc.pattern)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("literalRuns(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

func TestMatchQuery(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    string
		usable  bool
	}{
		{"%report%", `"report"`, true},
		{"%holiday%2024%", `"holiday" AND "2024"`, true},
		// Runs too short to have a trigram are dropped, which only widens the
		// candidate set - and the caller checks each one against the pattern.
		{"%a%report%", `"report"`, true},
		{"%budget_v2%", `"budget"`, true},
		// Nothing long enough to look up: the index cannot help.
		{"%ab%", "", false},
		{"%a%b%", "", false},
		{"%", "", false},
		// A quote in the search text must not end the phrase.
		{`%say "hi"%`, `"say ""hi"""`, true},
	} {
		got, ok := matchQuery(tc.pattern)
		if ok != tc.usable {
			t.Errorf("matchQuery(%q) usable = %v, want %v", tc.pattern, ok, tc.usable)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("matchQuery(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

// searchFixture builds an index holding names to search.
func searchFixture(t *testing.T, names ...string) (*DB, User) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ReconcileUsers(ctx, []UserMapping{{Username: "alice", Home: "/tmp/alice"}}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	u, _ := db.UserByName(ctx, "alice")

	stamp := Stamp()
	for i, name := range names {
		if _, err := UpsertNode(ctx, db, Node{
			UserID: u.ID, Path: fmt.Sprintf("dir%d/%s", i, name), Name: name, Size: 1,
		}, stamp); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	return db, u
}

func names(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func TestSearchNodesMatchesSubstrings(t *testing.T) {
	ctx := context.Background()
	db, u := searchFixture(t,
		"Quarterly Report 2024.pdf", "holiday-photo.jpg", "report-draft.txt", "REPORT-CAPS.txt")

	got, err := SearchNodes(ctx, db, u.ID, ".", "%report%", 100)
	if err != nil {
		t.Fatalf("SearchNodes: %v", err)
	}
	// LIKE is case-insensitive for ASCII, and a search box is expected to be.
	if len(got) != 3 {
		t.Errorf("search for report found %v, want the three reports", names(got))
	}
}

// TestSearchNodesFindsShortTerms: a term too short to have a trigram cannot use
// the index, and must still be answered rather than come back empty.
func TestSearchNodesFindsShortTerms(t *testing.T) {
	ctx := context.Background()
	db, u := searchFixture(t, "cv.pdf", "ab-notes.txt", "holiday.jpg")

	for _, tc := range []struct {
		pattern string
		want    int
	}{
		{"%cv%", 1},
		{"%ab%", 1},
		{"%a%", 2}, // ab-notes.txt and holiday.jpg
	} {
		got, err := SearchNodes(ctx, db, u.ID, ".", tc.pattern, 100)
		if err != nil {
			t.Fatalf("SearchNodes(%q): %v", tc.pattern, err)
		}
		if len(got) != tc.want {
			t.Errorf("search %q found %v, want %d", tc.pattern, names(got), tc.want)
		}
	}
}

// TestSearchNodesHonoursEscapedWildcards: the index prefilter must not widen
// the answer, only the candidates.
func TestSearchNodesHonoursEscapedWildcards(t *testing.T) {
	ctx := context.Background()
	db, u := searchFixture(t, "50% off sale.pdf", "50 off sale.pdf")

	got, err := SearchNodes(ctx, db, u.ID, ".", `%50\% off%`, 100)
	if err != nil {
		t.Fatalf("SearchNodes: %v", err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0].Name, "50%") {
		t.Errorf("search found %v, want only the name with a percent sign", names(got))
	}
}

// TestSearchIndexUsesTheTrigramIndex is the point of the whole thing. Without
// it the query still returns the right rows, just by reading every one of them,
// and nothing else in the tests would notice.
func TestSearchIndexUsesTheTrigramIndex(t *testing.T) {
	ctx := context.Background()
	db, u := searchFixture(t, "Quarterly Report 2024.pdf")

	rows, err := db.QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT `+qualifiedNodeColumns+`
		FROM nodes JOIN node_names ON node_names.rowid = nodes.id
		WHERE node_names MATCH ? AND nodes.user_id = ? AND nodes.name LIKE ? ESCAPE '\'
		ORDER BY nodes.path LIMIT ?`, `"report"`, u.ID, "%report%", 10)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "node_names") {
		t.Errorf("the search does not use the trigram index; plan was:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "SCAN nodes") {
		t.Errorf("the search still scans every row; plan was:\n%s", plan.String())
	}
}

// TestSearchIndexFollowsWrites checks the triggers. The index is maintained by
// the database rather than by the code that writes nodes, precisely so that no
// write path can forget - so the test drives the write paths, not the triggers.
func TestSearchIndexFollowsWrites(t *testing.T) {
	ctx := context.Background()
	db, u := searchFixture(t, "aardvark-lease.pdf")

	found := func(term string) int {
		t.Helper()
		got, err := SearchNodes(ctx, db, u.ID, ".", "%"+term+"%", 100)
		if err != nil {
			t.Fatalf("SearchNodes(%q): %v", term, err)
		}
		return len(got)
	}

	if found("aardvark") != 1 {
		t.Fatal("a newly indexed file was not searchable")
	}

	// Renamed: findable under the new name, and not the old one.
	node, err := NodeByPath(ctx, db, u.ID, "dir0/aardvark-lease.pdf")
	if err != nil {
		t.Fatalf("look up: %v", err)
	}
	if err := MoveNode(ctx, db, u.ID, node.Path, "dir0/buffalo-lease.pdf",
		node.ParentID, "buffalo-lease.pdf"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if found("buffalo") != 1 {
		t.Error("a renamed file was not searchable under its new name")
	}
	if found("aardvark") != 0 {
		t.Error("a renamed file is still searchable under its old name")
	}

	// Deleted: gone from the search too, or it would offer files that are not
	// there.
	if err := DeleteNode(ctx, db, u.ID, "dir0/buffalo-lease.pdf"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if found("buffalo") != 0 {
		t.Error("a deleted file is still searchable")
	}
}

// TestSearchIndexDoesNotChurnOnRescan: a rescan rewrites every row it visits,
// and reindexing names that did not change would cost more than the search
// saves.
func TestSearchIndexDoesNotChurnOnRescan(t *testing.T) {
	ctx := context.Background()
	db, u := searchFixture(t, "holiday-photo.jpg")

	size := func() int64 {
		t.Helper()
		var n int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM node_names_data`).Scan(&n); err != nil {
			t.Fatalf("read index size: %v", err)
		}
		return n
	}

	before := size()
	stamp := Stamp()
	for range 20 {
		if _, err := UpsertNode(ctx, db, Node{
			UserID: u.ID, Path: "dir0/holiday-photo.jpg", Name: "holiday-photo.jpg", Size: 2,
		}, stamp); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	if after := size(); after != before {
		t.Errorf("rewriting an unchanged name grew the search index from %d to %d rows", before, after)
	}
}

// TestNodeColumnListsAgree: the two column lists are read by the same scanner,
// so they must name the same columns in the same order.
func TestNodeColumnListsAgree(t *testing.T) {
	stripped := strings.ReplaceAll(qualifiedNodeColumns, "nodes.", "")
	if stripped != nodeColumns {
		t.Errorf("column lists have drifted:\n plain     = %s\n qualified = %s", nodeColumns, stripped)
	}
}

// TestSearchIndexBackfillsExistingRows covers the upgrade, which is the case
// that actually runs on a live server: the index is created over an account
// that already has millions of rows in it, and none of them arrive through the
// insert trigger.
func TestSearchIndexBackfillsExistingRows(t *testing.T) {
	ctx := context.Background()
	db, u := searchFixture(t, "aardvark-lease.pdf", "holiday-photo.jpg")

	// Put the database back the way it looked before this migration, with the
	// rows still in place.
	for _, stmt := range []string{
		`DROP TRIGGER nodes_search_insert`,
		`DROP TRIGGER nodes_search_delete`,
		`DROP TRIGGER nodes_search_update`,
		`DROP TABLE node_names`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// The search index migration is the last one; running it again is what an
	// upgrade does.
	if _, err := db.ExecContext(ctx, migrations[len(migrations)-1]); err != nil {
		t.Fatalf("re-apply the search migration: %v", err)
	}

	for _, term := range []string{"aardvark", "holiday"} {
		got, err := SearchNodes(ctx, db, u.ID, ".", "%"+term+"%", 100)
		if err != nil {
			t.Fatalf("SearchNodes(%q): %v", term, err)
		}
		if len(got) != 1 {
			t.Errorf("after the upgrade, %q found %v, want the one file", term, names(got))
		}
	}

	// And writes after the upgrade still reach it.
	if _, err := UpsertNode(ctx, db, Node{
		UserID: u.ID, Path: "dir9/buffalo-notes.txt", Name: "buffalo-notes.txt",
	}, Stamp()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := SearchNodes(ctx, db, u.ID, ".", "%buffalo%", 100)
	if err != nil {
		t.Fatalf("SearchNodes: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("a file added after the upgrade was not searchable")
	}
}
