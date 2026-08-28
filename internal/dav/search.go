package dav

import (
	"encoding/xml"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// SearchPath is the endpoint clients send WebDAV SEARCH to.
const SearchPath = "/remote.php/dav"

// defaultSearchLimit bounds a search that does not ask for one. A phone showing
// results a few at a time has no use for ten thousand of them, and the query
// walks the index.
const defaultSearchLimit = 100

// maxSearchLimit bounds what a client may ask for.
const maxSearchLimit = 1000

// searchRequest is the parsed body of a SEARCH.
type searchRequest struct {
	XMLName xml.Name `xml:"DAV: searchrequest"`
	Basic   struct {
		Select struct {
			Prop propContainer `xml:"DAV: prop"`
		} `xml:"DAV: select"`
		From struct {
			Scope []struct {
				Href  string `xml:"DAV: href"`
				Depth string `xml:"DAV: depth"`
			} `xml:"DAV: scope"`
		} `xml:"DAV: from"`
		Where struct {
			Inner string `xml:",innerxml"`
		} `xml:"DAV: where"`
		Limit struct {
			NResults int `xml:"DAV: nresults"`
		} `xml:"DAV: limit"`
	} `xml:"DAV: basicsearch"`
}

// literalRe pulls the pattern a search is looking for out of the where clause.
//
// The where clause is a small expression language of nested comparisons. Rather
// than implement it, Mirage takes the pattern out of it and matches names.
// That is what the search boxes in the clients ask for - somebody typing part
// of a filename - and answering that well is worth more than answering every
// expressible query badly. Anything else is reported as unsupported rather than
// quietly returning nothing, which a person would read as "it is not there".
var literalRe = regexp.MustCompile(`(?s)<(?:\w+:)?literal[^>]*>(.*?)</(?:\w+:)?literal>`)

// propRe finds which properties a where clause compares against.
var propRe = regexp.MustCompile(`<(?:\w+:)?(displayname|getcontenttype|getlastmodified|fileid|is-collection)\s*/?>`)

// maxPatternLen bounds the pattern. A long one full of wildcards makes LIKE
// backtrack, and no real search box produces one.
const maxPatternLen = 256

// Searching by name is served by a trigram index, so a term of three
// characters or more is found without reading the account. A shorter one has
// no index entry to look up and falls back to reading every row - correct, but
// slow on a large share.

// handleSearch answers a WebDAV SEARCH.
func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	user := auth.MustUser(r.Context())

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPropfindBody))
	if err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}
	var req searchRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, "malformed SEARCH body", http.StatusBadRequest)
		return
	}

	scope, ok := h.searchScope(req, user)
	if !ok {
		h.log.Warn("search outside the account's files refused",
			"user", user.Username, "scopes", len(req.Basic.From.Scope))
		http.NotFound(w, r)
		return
	}

	pattern, ok := searchPattern(req.Basic.Where.Inner)
	if !ok {
		// Better to say so than to answer an unsupported query with an empty
		// result, which a person reads as "it is not there".
		h.log.Warn("unsupported SEARCH query",
			"user", user.Username, "where", truncateForLog(req.Basic.Where.Inner))
		http.Error(w, "this server supports searching by name", http.StatusNotImplemented)
		return
	}

	limit := req.Basic.Limit.NResults
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	limit = min(limit, maxSearchLimit)

	names := req.Basic.Select.Prop.Names
	props := make([]PropName, 0, len(names))
	for _, n := range names {
		props = append(props, PropName{Space: n.XMLName.Space, Local: n.XMLName.Local})
	}
	if len(props) == 0 {
		props = allPropNames
	}

	matches, err := store.SearchNodes(r.Context(), h.db, user.ID, scope, pattern, limit)
	if err != nil {
		h.internalError(w, "search", err)
		return
	}
	usage, err := store.UserUsage(r.Context(), h.db, user.ID)
	if err != nil {
		h.internalError(w, "read usage", err)
		return
	}

	h.log.Debug("search",
		"user", user.Username, "scope", scope, "pattern", pattern, "results", len(matches))

	ms := newMultistatus(w)
	for _, n := range matches {
		h.writeNode(ms, user, n, usage, props)
	}
	if err := ms.close(); err != nil {
		h.log.Warn("could not finish the search response", "user", user.Username, "error", err)
	}
}

// searchScope maps the requested scope onto a path inside the account, and
// refuses one that points anywhere else.
func (h *Handler) searchScope(req searchRequest, user store.User) (string, bool) {
	if len(req.Basic.From.Scope) == 0 {
		return fsx.RootPath, true
	}
	href := req.Basic.From.Scope[0].Href

	// Clients write this either as a full DAV path or as /files/<user>/...
	rest := href
	if trimmed, ok := strings.CutPrefix(rest, FilesPrefix); ok {
		rest = trimmed
	} else if trimmed, ok := strings.CutPrefix(rest, "/files/"); ok {
		rest = trimmed
	} else {
		return "", false
	}

	owner, tail, _ := strings.Cut(strings.Trim(rest, "/"), "/")
	if owner != user.Username {
		return "", false
	}
	scope, err := fsx.CleanPath(tail)
	if err != nil {
		return "", false
	}
	return scope, true
}

// searchPattern extracts the pattern being searched for.
//
// A DAV literal in a d:like carries the same wildcards as SQL LIKE - % and _,
// escaped with a backslash - so it is passed through rather than reinterpreted.
// The clients send %term%. A pattern with no wildcards at all is taken as a
// substring search, which is what somebody typing into a box means even if
// their client sent it literally.
func searchPattern(where string) (string, bool) {
	if strings.TrimSpace(where) == "" {
		return "", false
	}
	// Only name searches are answered; a query about content types or dates
	// would need the expression language this deliberately does not implement.
	for _, p := range propRe.FindAllStringSubmatch(where, -1) {
		if p[1] != "displayname" {
			return "", false
		}
	}
	m := literalRe.FindStringSubmatch(where)
	if m == nil {
		return "", false
	}
	pattern := html.UnescapeString(m[1])
	if strings.TrimSpace(pattern) == "" || len(pattern) > maxPatternLen {
		return "", false
	}
	if !strings.ContainsAny(pattern, "%_") {
		pattern = "%" + pattern + "%"
	}
	return pattern, true
}

func truncateForLog(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 200 {
		return s
	}
	return s[:199] + "…"
}
