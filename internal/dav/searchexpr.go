package dav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/store"
)

// The where clause of a SEARCH is a small expression language: nested and, or
// and not around comparisons of a property against a literal. It is compiled
// here into a SQL fragment rather than evaluated in Go, because the alternative
// is reading every row in the account to filter it.
//
// Only the properties the index actually holds can be compared. Anything else
// is refused rather than quietly ignored: dropping a term from a query silently
// widens the result, and a person searching for photographs from last summer
// would be handed the whole share.

// maxExprNodes bounds a query. Deeply nested expressions come from the network
// and the compiler recurses over them.
const maxExprNodes = 200

// condition is a compiled where clause.
type condition struct {
	SQL  string
	Args []any
	// NameLiterals are substrings the entry's name must contain, collected so
	// the trigram index can narrow the candidates before the rest is evaluated.
	NameLiterals []string
}

// exprNode is the generic XML tree a where clause parses into.
type exprNode struct {
	XMLName  xml.Name
	Children []exprNode `xml:",any"`
	Chardata string     `xml:",chardata"`
}

// searchColumn describes a property the index can compare.
type searchColumn struct {
	sql  string
	kind columnKind
}

type columnKind int

const (
	kindText columnKind = iota
	kindInt
	kindTime
	kindBool
)

// searchColumns maps the properties clients search on to index columns.
//
// getcontenttype is what the media view in the mobile apps filters on, and
// getlastmodified is how it pages through by date; without those two that view
// shows nothing at all.
var searchColumns = map[string]searchColumn{
	"displayname":      {"nodes.name", kindText},
	"getcontenttype":   {"nodes.content_type", kindText},
	"getlastmodified":  {"nodes.mtime", kindTime},
	"getcontentlength": {"nodes.size", kindInt},
	"size":             {"nodes.size", kindInt},
	"fileid":           {"nodes.id", kindInt},
	"is-collection":    {"nodes.is_dir", kindBool},
}

// compileWhere turns a where clause into SQL.
func compileWhere(inner string) (condition, error) {
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return condition{}, nil
	}
	var root exprNode
	// Wrapped so that a clause with several top-level terms parses as one tree.
	wrapped := `<where xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" ` +
		`xmlns:nc="http://nextcloud.org/ns">` + trimmed + `</where>`
	if err := xml.Unmarshal([]byte(wrapped), &root); err != nil {
		return condition{}, fmt.Errorf("malformed where clause: %w", err)
	}

	terms := elements(root.Children)
	if len(terms) == 0 {
		return condition{}, nil
	}
	c := &compiler{}
	sql, err := c.compileAll(terms, " AND ")
	if err != nil {
		return condition{}, err
	}
	return condition{SQL: sql, Args: c.args, NameLiterals: c.nameLiterals}, nil
}

type compiler struct {
	args         []any
	nameLiterals []string
	nodes        int
}

// compileAll joins several terms with one operator.
func (c *compiler) compileAll(nodes []exprNode, join string) (string, error) {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		sql, err := c.compile(n)
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, join) + ")", nil
}

func (c *compiler) compile(n exprNode) (string, error) {
	c.nodes++
	if c.nodes > maxExprNodes {
		return "", errors.New("the query is too complex")
	}

	kids := elements(n.Children)
	switch n.XMLName.Local {
	case "and":
		if len(kids) == 0 {
			return "1", nil
		}
		return c.compileAll(kids, " AND ")
	case "or":
		if len(kids) == 0 {
			return "0", nil
		}
		return c.compileAll(kids, " OR ")
	case "not":
		if len(kids) != 1 {
			return "", errors.New("not takes exactly one term")
		}
		sql, err := c.compile(kids[0])
		if err != nil {
			return "", err
		}
		return "(NOT " + sql + ")", nil
	case "is-collection":
		return "nodes.is_dir = 1", nil
	case "is-defined":
		col, err := c.column(kids)
		if err != nil {
			return "", err
		}
		return "(" + col.sql + " IS NOT NULL AND " + col.sql + " <> '')", nil
	case "eq", "lt", "gt", "lte", "gte", "like":
		return c.comparison(n.XMLName.Local, kids)
	default:
		return "", fmt.Errorf("cannot search on %q", n.XMLName.Local)
	}
}

// comparison compiles one property-against-literal term.
func (c *compiler) comparison(op string, kids []exprNode) (string, error) {
	col, err := c.column(kids)
	if err != nil {
		return "", err
	}
	literal, ok := findLiteral(kids)
	if !ok {
		return "", fmt.Errorf("%s needs a literal to compare against", op)
	}

	if op == "like" {
		if col.kind != kindText {
			return "", errors.New("like only compares text")
		}
		// The literal is already a LIKE pattern; the DAV wildcards are the SQL
		// ones. A pattern with no wildcards at all is taken as a substring
		// search, which is what a client sending a bare word means even though
		// the letter of the specification says it should match exactly.
		if !strings.ContainsAny(literal, "%_") {
			literal = "%" + literal + "%"
		}
		// The literal runs are collected so the trigram index can be used when
		// the property is the name.
		if col.sql == "nodes.name" {
			c.nameLiterals = append(c.nameLiterals, store.LikeLiterals(literal)...)
		}
		c.args = append(c.args, literal)
		return col.sql + ` LIKE ? ESCAPE '\'`, nil
	}

	value, err := convertLiteral(col.kind, literal)
	if err != nil {
		return "", err
	}
	c.args = append(c.args, value)
	return col.sql + " " + sqlOperator(op) + " ?", nil
}

// column finds the property a comparison is about.
func (c *compiler) column(kids []exprNode) (searchColumn, error) {
	for _, k := range kids {
		if k.XMLName.Local != "prop" {
			continue
		}
		for _, p := range elements(k.Children) {
			col, ok := searchColumns[p.XMLName.Local]
			if !ok {
				return searchColumn{}, fmt.Errorf("cannot search on %s", p.XMLName.Local)
			}
			return col, nil
		}
	}
	return searchColumn{}, errors.New("no property named")
}

func findLiteral(kids []exprNode) (string, bool) {
	for _, k := range kids {
		if k.XMLName.Local == "literal" {
			return strings.TrimSpace(k.Chardata), true
		}
	}
	return "", false
}

func sqlOperator(op string) string {
	switch op {
	case "eq":
		return "="
	case "lt":
		return "<"
	case "gt":
		return ">"
	case "lte":
		return "<="
	case "gte":
		return ">="
	}
	return "="
}

// convertLiteral turns the text of a literal into the type its column holds.
func convertLiteral(kind columnKind, literal string) (any, error) {
	switch kind {
	case kindInt:
		n, err := strconv.ParseInt(literal, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", literal)
		}
		return n, nil
	case kindBool:
		return boolLiteral(literal), nil
	case kindTime:
		t, err := parseSearchTime(literal)
		if err != nil {
			return nil, err
		}
		return t.Unix(), nil
	default:
		return literal, nil
	}
}

func boolLiteral(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return 1
	}
	return 0
}

// searchTimeFormats are the shapes a date literal arrives in. Clients are not
// consistent about this, and a rejected date means an empty gallery.
var searchTimeFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	time.RFC1123,
	time.RFC1123Z,
}

func parseSearchTime(literal string) (time.Time, error) {
	literal = strings.TrimSpace(literal)
	// Some clients send a plain unix timestamp.
	if n, err := strconv.ParseInt(literal, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	for _, f := range searchTimeFormats {
		if t, err := time.Parse(f, literal); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date this server understands", literal)
}

// elements drops the whitespace and comments between tags.
func elements(nodes []exprNode) []exprNode {
	out := make([]exprNode, 0, len(nodes))
	for _, n := range nodes {
		if n.XMLName.Local != "" {
			out = append(out, n)
		}
	}
	return out
}

// orderClause compiles the orderby of a search into a SQL fragment.
//
// Whitelisted rather than interpolated, so the ordering a client asks for
// cannot become a way to write SQL.
func orderClause(inner string) (string, error) {
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return "nodes.path", nil
	}
	var root exprNode
	wrapped := `<orderby xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">` + trimmed + `</orderby>`
	if err := xml.Unmarshal([]byte(wrapped), &root); err != nil {
		return "nodes.path", nil //nolint:nilerr // an unreadable ordering is not worth failing over
	}

	var parts []string
	for _, order := range elements(root.Children) {
		if order.XMLName.Local != "order" {
			continue
		}
		kids := elements(order.Children)
		var col searchColumn
		var found bool
		descending := false
		for _, k := range kids {
			switch k.XMLName.Local {
			case "prop":
				for _, p := range elements(k.Children) {
					if c, ok := searchColumns[p.XMLName.Local]; ok {
						col, found = c, true
					}
				}
			case "descending":
				descending = true
			}
		}
		if !found {
			continue
		}
		if descending {
			parts = append(parts, col.sql+" DESC")
			continue
		}
		parts = append(parts, col.sql+" ASC")
	}
	if len(parts) == 0 {
		return "nodes.path", nil
	}
	// A stable tiebreak, or paging through equal timestamps repeats rows.
	parts = append(parts, "nodes.path")
	return strings.Join(parts, ", "), nil
}
