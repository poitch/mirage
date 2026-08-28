package ocs

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/dav"
	"github.com/poitch/mirage/internal/store"
)

// filesProviderID is the only search provider Mirage offers. The clients look
// for this name specifically when deciding how to present a result, so it is
// not free to choose.
const filesProviderID = "files"

const (
	// defaultUnifiedLimit is what a client gets when it does not say. The
	// search bar shows a handful and asks for more if somebody wants them.
	defaultUnifiedLimit = 20
	maxUnifiedLimit     = 200
)

// searchProvider describes a source of search results.
type searchProvider struct {
	ID          string            `xml:"id" json:"id"`
	AppID       string            `xml:"appId" json:"appId"`
	Name        string            `xml:"name" json:"name"`
	Icon        string            `xml:"icon" json:"icon"`
	Order       int               `xml:"order" json:"order"`
	Triggers    []string          `xml:"triggers>element" json:"triggers"`
	Filters     map[string]string `xml:"-" json:"filters"`
	InAppSearch bool              `xml:"inAppSearch" json:"inAppSearch"`
}

// searchResult is one provider's answer.
type searchResult struct {
	Name        string        `xml:"name" json:"name"`
	IsPaginated bool          `xml:"isPaginated" json:"isPaginated"`
	Entries     []searchEntry `xml:"entries>element" json:"entries"`
	// Cursor is where to continue from, or null when there is no more. It is a
	// pointer so that "no more results" is null rather than an empty string,
	// which a client would read as a valid cursor and ask again forever.
	Cursor *string `xml:"cursor" json:"cursor"`
}

type searchEntry struct {
	ThumbnailURL string            `xml:"thumbnailUrl" json:"thumbnailUrl"`
	Title        string            `xml:"title" json:"title"`
	Subline      string            `xml:"subline" json:"subline"`
	ResourceURL  string            `xml:"resourceUrl" json:"resourceUrl"`
	Icon         string            `xml:"icon" json:"icon"`
	Rounded      bool              `xml:"rounded" json:"rounded"`
	Attributes   map[string]string `xml:"-" json:"attributes"`
}

// SearchProviders lists what can be searched.
//
// The desktop client asks this before it will offer a search box at all, which
// is why searching from the desktop failed while the WebDAV SEARCH the mobile
// apps use worked: they are two unrelated APIs.
func (s *Service) SearchProviders(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = auth.MustUser(r.Context())
		Write(w, r, v, []searchProvider{{
			ID:       filesProviderID,
			AppID:    filesProviderID,
			Name:     "Files",
			Order:    5,
			Triggers: []string{},
			// Only a plain term. Advertising a filter Mirage cannot apply would
			// have clients offer a control that silently does nothing.
			Filters:     map[string]string{"term": "string"},
			InAppSearch: false,
		}})
	}
}

// Search answers one provider's search.
func (s *Service) Search(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.MustUser(r.Context())

		if id := r.PathValue("providerId"); id != filesProviderID {
			WriteError(w, r, v, StatusNotFound, "no search provider "+strconv.Quote(id))
			return
		}

		term := strings.TrimSpace(r.URL.Query().Get("term"))
		if term == "" {
			Write(w, r, v, searchResult{Name: "Files", Entries: []searchEntry{}})
			return
		}

		limit := defaultUnifiedLimit
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
			limit = min(n, maxUnifiedLimit)
		}
		// The cursor is the path of the last result handed out, so a follow-up
		// asks for what sorts after it.
		cursor := r.URL.Query().Get("cursor")

		// One more than asked for, so that whether a further page exists is
		// known without a second query.
		matches, err := store.SearchNodes(r.Context(), s.db, user.ID, ".",
			store.SubstringPattern(term), cursor, limit+1)
		if err != nil {
			s.log.Error("search failed", "user", user.Username, "error", err)
			WriteError(w, r, v, StatusError, "the search could not be run")
			return
		}

		var next *string
		if len(matches) > limit {
			matches = matches[:limit]
			last := matches[len(matches)-1].Path
			next = &last
		}

		entries := make([]searchEntry, 0, len(matches))
		for _, n := range matches {
			entries = append(entries, s.searchEntryFor(user, n))
		}

		s.log.Debug("unified search",
			"user", user.Username, "term", term, "results", len(entries))

		Write(w, r, v, searchResult{
			Name:        "Files",
			IsPaginated: true,
			Entries:     entries,
			Cursor:      next,
		})
	}
}

// searchEntryFor renders one match the way a client expects to receive it.
func (s *Service) searchEntryFor(user store.User, n store.Node) searchEntry {
	// The folder the match sits in, which is what tells two files of the same
	// name apart in a list.
	subline := path.Dir(n.Path)
	if subline == "." || subline == "/" {
		subline = "/"
	} else {
		subline = "/" + subline
	}

	icon := "icon-file"
	if n.IsDir {
		icon = "icon-folder"
	}

	return searchEntry{
		Title:       n.Name,
		Subline:     subline,
		ResourceURL: s.externalURL + FilePath(n.ID),
		Icon:        icon,
		Attributes: map[string]string{
			"fileId": strconv.FormatInt(n.ID, 10),
			"path":   "/" + n.Path,
		},
	}
}

// FilePath is the address of a file by its id, which is what a search result
// links to. Nextcloud opens it in the web interface; Mirage has none, so it
// redirects to the file itself.
func FilePath(fileID int64) string {
	return "/index.php/f/" + strconv.FormatInt(fileID, 10)
}

// OpenFile resolves /index.php/f/{fileid} to the file it names.
//
// Search results have to link somewhere. Nextcloud points at a web interface
// Mirage does not have, so this redirects to the file's WebDAV address, which
// is a real thing a browser or a client can follow.
func (s *Service) OpenFile(w http.ResponseWriter, r *http.Request) {
	user := auth.MustUser(r.Context())

	id, err := strconv.ParseInt(r.PathValue("fileid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Scoped to the caller by the query itself, so another account's file is
	// simply not found rather than refused - whether it exists is not something
	// this should confirm.
	node, err := store.NodeByID(r.Context(), s.db, user.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.log.Error("could not look up a file by id", "file_id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, dav.Href(user.Username, node.Path, node.IsDir), http.StatusFound)
}
