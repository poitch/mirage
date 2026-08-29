package dav

import (
	"strings"
	"testing"
)

func TestCompileWhereMediaFilter(t *testing.T) {
	// The shape the media view in the mobile apps sends: a content-type filter
	// crossed with a date range. It was refused outright before, which is why
	// that view showed nothing.
	where := `
	<d:and>
	  <!-- Media type filter -->
	  <d:or>
	    <d:like><d:prop><d:getcontenttype/></d:prop><d:literal>image/%</d:literal></d:like>
	    <d:like><d:prop><d:getcontenttype/></d:prop><d:literal>video/%</d:literal></d:like>
	  </d:or>
	  <d:and>
	    <d:lt><d:prop><d:getlastmodified/></d:prop><d:literal>2026-08-29T00:00:00Z</d:literal></d:lt>
	    <d:gt><d:prop><d:getlastmodified/></d:prop><d:literal>2020-01-01T00:00:00Z</d:literal></d:gt>
	  </d:and>
	</d:and>`

	got, err := compileWhere(where)
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	for _, want := range []string{"nodes.content_type LIKE ?", "nodes.mtime <", "nodes.mtime >", " OR ", " AND "} {
		if !strings.Contains(got.SQL, want) {
			t.Errorf("compiled SQL is missing %q:\n%s", want, got.SQL)
		}
	}
	if len(got.Args) != 4 {
		t.Fatalf("got %d arguments, want 4: %v", len(got.Args), got.Args)
	}
	if got.Args[0] != "image/%" || got.Args[1] != "video/%" {
		t.Errorf("content type arguments = %v", got.Args[:2])
	}
	// Dates become unix seconds, which is how the index stores them.
	if _, ok := got.Args[2].(int64); !ok {
		t.Errorf("date argument is %T, want int64", got.Args[2])
	}
	// No name literals here, so the trigram index cannot help and must not be
	// asked to: requiring a match would return nothing.
	if len(got.NameLiterals) != 0 {
		t.Errorf("collected name literals from a query with no name term: %v", got.NameLiterals)
	}
}

func TestCompileWhereNameSearch(t *testing.T) {
	where := `<d:like><d:prop><d:displayname/></d:prop><d:literal>%holiday%</d:literal></d:like>`
	got, err := compileWhere(where)
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	if !strings.Contains(got.SQL, "nodes.name LIKE ?") {
		t.Errorf("SQL = %q", got.SQL)
	}
	// The literal run is collected so the trigram index narrows the candidates.
	if len(got.NameLiterals) != 1 || got.NameLiterals[0] != "holiday" {
		t.Errorf("name literals = %v, want [holiday]", got.NameLiterals)
	}
}

func TestCompileWhereNegationAndCollections(t *testing.T) {
	got, err := compileWhere(`<d:not><d:is-collection/></d:not>`)
	if err != nil {
		t.Fatalf("compileWhere: %v", err)
	}
	if !strings.Contains(got.SQL, "NOT") || !strings.Contains(got.SQL, "is_dir") {
		t.Errorf("SQL = %q", got.SQL)
	}
}

// TestCompileWhereRefusesWhatItCannotAnswer: dropping a term it does not
// understand would silently widen the result, and somebody searching for last
// summer's photographs would be handed the whole share.
func TestCompileWhereRefusesWhatItCannotAnswer(t *testing.T) {
	for name, where := range map[string]string{
		"an unknown property": `<d:eq><d:prop><oc:favorite/></d:prop><d:literal>1</d:literal></d:eq>`,
		"an unknown operator": `<d:matches><d:prop><d:displayname/></d:prop><d:literal>x</d:literal></d:matches>`,
		"a bad date":          `<d:gt><d:prop><d:getlastmodified/></d:prop><d:literal>whenever</d:literal></d:gt>`,
		"a bad number":        `<d:gt><d:prop><d:getcontentlength/></d:prop><d:literal>big</d:literal></d:gt>`,
		"like on a date":      `<d:like><d:prop><d:getlastmodified/></d:prop><d:literal>2024%</d:literal></d:like>`,
		"no literal":          `<d:eq><d:prop><d:displayname/></d:prop></d:eq>`,
	} {
		if _, err := compileWhere(where); err == nil {
			t.Errorf("compileWhere accepted %s", name)
		}
	}
}

func TestCompileWhereBoundsComplexity(t *testing.T) {
	deep := strings.Repeat("<d:not>", 300) + `<d:is-collection/>` + strings.Repeat("</d:not>", 300)
	if _, err := compileWhere(deep); err == nil {
		t.Error("compileWhere accepted an unboundedly nested query")
	}
}

func TestOrderClause(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"newest first": {
			`<d:order><d:prop><d:getlastmodified/></d:prop><d:descending/></d:order>`,
			"nodes.mtime DESC, nodes.path",
		},
		"oldest first": {
			`<d:order><d:prop><d:getlastmodified/></d:prop><d:ascending/></d:order>`,
			"nodes.mtime ASC, nodes.path",
		},
		"empty": {"", "nodes.path"},
		// An ordering on something the index does not hold falls back rather
		// than failing: the results are still right, just differently sorted.
		"unknown property": {
			`<d:order><d:prop><oc:favorite/></d:prop></d:order>`, "nodes.path",
		},
	} {
		got, err := orderClause(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: orderClause = %q, want %q", name, got, tc.want)
		}
	}
}

// TestOrderClauseCannotInjectSQL: the ordering comes from the network and is
// pasted into a query, so it is whitelisted rather than quoted.
func TestOrderClauseCannotInjectSQL(t *testing.T) {
	got, err := orderClause(
		`<d:order><d:prop><d:x-evil>1; DROP TABLE nodes; --</d:x-evil></d:prop></d:order>`)
	if err != nil {
		t.Fatalf("orderClause: %v", err)
	}
	if got != "nodes.path" {
		t.Errorf("orderClause = %q, want the safe default", got)
	}
}
