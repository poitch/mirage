package store

import (
	"context"
	"strings"
)

// qualifiedNodeColumns names the same columns as nodeColumns but against their
// table, which the search needs because it joins the index and both tables have
// a name column. Written out rather than derived: the two must stay in the same
// order as scanNode reads them, and a clever transformation would hide that.
const qualifiedNodeColumns = `nodes.id, nodes.user_id, COALESCE(nodes.parent_id, 0), nodes.path, ` +
	`nodes.name, nodes.is_dir, nodes.size, nodes.mtime, nodes.etag, nodes.content_type, ` +
	`nodes.dev, nodes.inode, nodes.scanned_at, nodes.complete`

// SubstringPattern turns text somebody typed into a LIKE pattern that finds it
// anywhere in a name.
//
// The wildcards in the text are escaped, because a person searching for "50%"
// means those characters and not "anything at all".
func SubstringPattern(term string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term)
	return "%" + escaped + "%"
}

// trigramMin is the shortest run of text the index can find.
//
// The index records every three-letter run, so a run of one or two characters
// has no entry to look up. Searches for less than that fall back to reading the
// account, which is slow but rare - and still correct, which matters more.
const trigramMin = 3

// SearchNodes finds entries whose name matches a LIKE pattern, within a scope.
//
// The match is on the name rather than the full path, which is what somebody
// typing into a search box means.
//
// A substring match is not something an ordinary index can serve, so a trigram
// index narrows the candidates first and the LIKE then checks them. The LIKE is
// what decides the answer either way, so the index can be missing a row, or
// offer one that does not match, without the result changing - it only makes
// the search fast. That is deliberate: a search index that has to be perfect to
// be correct is one that silently returns the wrong thing when it drifts.
func SearchNodes(ctx context.Context, q Querier, userID int64, scope, pattern, after string, limit int) ([]Node, error) {
	where := []string{"nodes.user_id = ?", `nodes.name LIKE ? ESCAPE '\'`}
	args := []any{userID, pattern}

	// Results are ordered by path, so continuing from the last one seen is a
	// matter of asking for what sorts after it. That stays correct as rows are
	// added or removed between pages, which an offset would not.
	if after != "" {
		where = append(where, "nodes.path > ?")
		args = append(args, after)
	}

	// Scoping with a range over the path index is more selective than anything
	// the name index can offer, and costs nothing to add.
	if scope != "." && scope != "" {
		lo, hi, ok := PrefixRange(scope + "/")
		if !ok {
			return nil, nil
		}
		where = append(where, "nodes.path >= ?", "nodes.path < ?")
		args = append(args, lo, hi)
	} else {
		where = append(where, "nodes.path <> '.'")
	}

	from := "nodes"
	if match, ok := matchQuery(pattern); ok {
		// The join must come first so its arguments are bound in order.
		from = "nodes JOIN node_names ON node_names.rowid = nodes.id"
		where = append([]string{"node_names MATCH ?"}, where...)
		args = append([]any{match}, args...)
	}

	args = append(args, limit)
	rows, err := q.QueryContext(ctx, `
		SELECT `+qualifiedNodeColumns+` FROM `+from+`
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY nodes.path
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return collectNodes(rows)
}

// matchQuery turns a LIKE pattern into a trigram query that matches at least
// everything the pattern does, or reports that the index cannot help.
//
// Only the literal runs of the pattern are used, and only those long enough to
// have trigrams. Everything else is dropped, which can only widen the result -
// and the caller checks each candidate against the pattern itself, so widening
// is free. A pattern with no usable run at all, such as "%ab%", cannot be
// narrowed and is reported as such rather than answered wrongly.
func matchQuery(pattern string) (string, bool) {
	var phrases []string
	for _, run := range literalRuns(pattern) {
		if len([]rune(run)) >= trigramMin {
			phrases = append(phrases, `"`+strings.ReplaceAll(run, `"`, `""`)+`"`)
		}
	}
	if len(phrases) == 0 {
		return "", false
	}
	return strings.Join(phrases, " AND "), true
}

// literalRuns splits a LIKE pattern into the stretches of ordinary text between
// its wildcards, undoing the backslash escaping as it goes.
func literalRuns(pattern string) []string {
	var runs []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			runs = append(runs, cur.String())
			cur.Reset()
		}
	}

	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '\\':
			// An escaped wildcard is ordinary text. A trailing backslash is
			// malformed; treated as text, since the LIKE decides the answer.
			if i+1 < len(pattern) {
				i++
				cur.WriteByte(pattern[i])
				continue
			}
			cur.WriteByte(c)
		case '%', '_':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return runs
}
