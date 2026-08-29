package dav

import (
	"encoding/xml"
	"io"
	"net/http"
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
		OrderBy struct {
			Inner string `xml:",innerxml"`
		} `xml:"DAV: orderby"`
		Limit struct {
			NResults int `xml:"DAV: nresults"`
		} `xml:"DAV: limit"`
	} `xml:"DAV: basicsearch"`
}

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

	// The where clause is compiled to SQL rather than evaluated here: filtering
	// in Go would mean reading every entry in the account to throw most of them
	// away, and the media view in the mobile apps searches the whole share.
	cond, err := compileWhere(req.Basic.Where.Inner)
	if err != nil {
		// Said plainly rather than answered with an empty result, which a
		// person reads as "there is nothing there".
		h.log.Warn("unsupported SEARCH query",
			"user", user.Username, "error", err, "where", truncateForLog(req.Basic.Where.Inner))
		http.Error(w, "this server cannot answer that search: "+err.Error(),
			http.StatusNotImplemented)
		return
	}
	order, err := orderClause(req.Basic.OrderBy.Inner)
	if err != nil {
		http.Error(w, "this server cannot order a search that way", http.StatusNotImplemented)
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

	matches, err := store.Search(r.Context(), h.db, user.ID, store.SearchQuery{
		Scope:        scope,
		Where:        cond.SQL,
		Args:         cond.Args,
		NameLiterals: cond.NameLiterals,
		Order:        order,
		Limit:        limit,
	})
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
		"user", user.Username, "scope", scope, "results", len(matches))

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

func truncateForLog(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 200 {
		return s
	}
	return s[:199] + "…"
}
