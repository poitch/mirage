package dav

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// TrashPrefix is the WebDAV root for an account's deleted files.
const TrashPrefix = "/remote.php/dav/trashbin/"

// The two collections under it. Clients list the first and move entries into
// the second to restore them.
const (
	trashCollection   = "trash"
	restoreCollection = "restore"
)

// TrashHandler serves the trashbin endpoints.
type TrashHandler struct {
	*Handler
}

// Trashbin returns a handler for the trashbin, sharing this handler's
// dependencies.
func (h *Handler) Trashbin() *TrashHandler { return &TrashHandler{Handler: h} }

// ServeHTTP dispatches a trashbin request.
func (t *TrashHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := auth.MustUser(r.Context())
	if r.PathValue("user") != user.Username {
		// Answered as missing rather than refused, so this cannot be used to
		// find out whose accounts exist.
		http.NotFound(w, r)
		return
	}

	collection, name := splitTrashPath(r.PathValue("path"))
	switch r.Method {
	case "PROPFIND":
		if collection != trashCollection {
			http.NotFound(w, r)
			return
		}
		t.list(w, r, user, name)
	case http.MethodDelete:
		if collection != trashCollection {
			http.NotFound(w, r)
			return
		}
		t.remove(w, r, user, name)
	case "MOVE":
		if collection != trashCollection || name == "" {
			http.NotFound(w, r)
			return
		}
		t.restore(w, r, user, name)
	case http.MethodOptions:
		w.Header().Set("DAV", ComplianceClasses)
		w.Header().Set("Allow", "OPTIONS, PROPFIND, DELETE, MOVE")
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Allow", "OPTIONS, PROPFIND, DELETE, MOVE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// splitTrashPath separates the collection from the entry name.
func splitTrashPath(p string) (collection, name string) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", ""
	}
	collection, rest, _ := strings.Cut(p, "/")
	return collection, rest
}

// list answers a PROPFIND on the trash collection or one entry in it.
func (t *TrashHandler) list(w http.ResponseWriter, r *http.Request, user store.User, name string) {
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1"
	}
	if depth != "0" && depth != "1" && depth != "infinity" {
		http.Error(w, "Depth must be 0, 1 or infinity", http.StatusBadRequest)
		return
	}

	names, allProps, err := parsePropfind(r.Body)
	if err != nil {
		http.Error(w, "malformed PROPFIND body", http.StatusBadRequest)
		return
	}
	if allProps || len(names) == 0 {
		names = trashPropNames
	}

	entries, err := store.ListTrash(r.Context(), t.db, user.ID)
	if err != nil {
		t.internalError(w, "list trash", err)
		return
	}

	// One named entry rather than the whole collection.
	if name != "" {
		for _, e := range entries {
			if e.Name == name {
				ms := newMultistatus(w)
				t.writeEntry(ms, user, e, names)
				if err := ms.close(); err != nil {
					t.log.Warn("could not finish the trash listing", "error", err)
				}
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	ms := newMultistatus(w)
	// The collection itself, which clients expect to see first.
	ms.writeResponse(trashHref(user.Username, ""), []prop{
		{Name: PropName{Space: NSDav, Local: "resourcetype"}, Value: "<d:collection/>"},
		{Name: PropName{Space: NSDav, Local: "getlastmodified"},
			Value: escapeText(time.Now().UTC().Format(http.TimeFormat))},
	}, nil)

	if depth != "0" {
		for _, e := range entries {
			t.writeEntry(ms, user, e, names)
		}
	}
	if err := ms.close(); err != nil {
		t.log.Warn("could not finish the trash listing", "error", err)
	}
}

// trashPropNames are what a client gets when it asks for everything.
var trashPropNames = []PropName{
	{Space: NSDav, Local: "resourcetype"},
	{Space: NSDav, Local: "getcontentlength"},
	{Space: NSDav, Local: "getlastmodified"},
	{Space: NSDav, Local: "getcontenttype"},
	{Space: NSOwnCloud, Local: "fileid"},
	{Space: NSOwnCloud, Local: "size"},
	{Space: NSNextcloud, Local: "trashbin-filename"},
	{Space: NSNextcloud, Local: "trashbin-original-location"},
	{Space: NSNextcloud, Local: "trashbin-deletion-time"},
}

// writeEntry emits one deleted file.
func (t *TrashHandler) writeEntry(ms *multistatus, user store.User, e store.TrashEntry, names []PropName) {
	// The original name and location are the point of the listing: without them
	// a person sees a list of files with a timestamp glued to the end and no
	// idea which folder any of them came from.
	values := map[PropName]string{
		{Space: NSNextcloud, Local: "trashbin-filename"}:          escapeText(path.Base(e.OriginalPath)),
		{Space: NSNextcloud, Local: "trashbin-original-location"}: escapeText(e.OriginalPath),
		{Space: NSNextcloud, Local: "trashbin-deletion-time"}:     strconv.FormatInt(e.DeletedAt.Unix(), 10),
		{Space: NSOwnCloud, Local: "fileid"}:                      strconv.FormatInt(e.ID, 10),
		{Space: NSOwnCloud, Local: "size"}:                        strconv.FormatInt(e.Size, 10),
		{Space: NSDav, Local: "getlastmodified"}:                  escapeText(e.DeletedAt.UTC().Format(http.TimeFormat)),
	}
	if e.IsDir {
		values[PropName{Space: NSDav, Local: "resourcetype"}] = "<d:collection/>"
	} else {
		values[PropName{Space: NSDav, Local: "resourcetype"}] = ""
		values[PropName{Space: NSDav, Local: "getcontentlength"}] = strconv.FormatInt(e.Size, 10)
		values[PropName{Space: NSDav, Local: "getcontenttype"}] = escapeText(mimeTypeOf(e.OriginalPath))
	}

	var found []prop
	var missing []PropName
	for _, n := range names {
		if v, ok := values[n]; ok {
			found = append(found, prop{Name: n, Value: v})
			continue
		}
		missing = append(missing, n)
	}
	ms.writeResponse(trashHref(user.Username, e.Name), found, missing)
}

// restore moves an entry back to where it was deleted from.
func (t *TrashHandler) restore(w http.ResponseWriter, r *http.Request, user store.User, name string) {
	dest, err := t.restoreDestination(r, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	entry, err := store.TrashByName(r.Context(), t.db, user.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		t.internalError(w, "look up trash entry", err)
		return
	}

	// A client may name where to put it; by default it goes back where it came
	// from, which is what the restore button in every client means.
	target := entry.OriginalPath
	if dest != "" {
		target = dest
	}

	st, err := t.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		t.internalError(w, "open storage", err)
		return
	}

	// Something may be at the original path now. Restoring beside it is better
	// than either refusing, which leaves the person stuck, or overwriting,
	// which loses the file that is there.
	target, err = t.freeName(r, st, user, target)
	if err != nil {
		t.internalError(w, "choose a restore path", err)
		return
	}

	if err := st.RestoreFromTrash(entry.Name, target); err != nil {
		t.log.Warn("could not restore a deleted file",
			"user", user.Username, "entry", entry.Name, "target", target, "error", err)
		http.Error(w, "could not restore that file", http.StatusConflict)
		return
	}
	if err := store.RemoveTrashEntry(r.Context(), t.db, user.ID, entry.Name); err != nil {
		t.internalError(w, "forget trash entry", err)
		return
	}

	// Indexed now rather than at the next scan: a restored file that clients
	// cannot see until the next pass looks like the restore failed.
	if err := t.scanner.ScanPath(r.Context(), user, target); err != nil {
		t.log.Warn("restored a file but could not index it",
			"user", user.Username, "path", target, "error", err)
	}

	t.log.Info("restored a deleted file",
		"user", user.Username, "path", target, "deleted_at", entry.DeletedAt)
	w.WriteHeader(http.StatusCreated)
}

// restoreDestination reads the Destination header, which must point into this
// account's restore collection.
func (t *TrashHandler) restoreDestination(r *http.Request, user store.User) (string, error) {
	raw := r.Header.Get("Destination")
	if raw == "" {
		return "", errors.New("MOVE requires a Destination")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("Destination is not a URL")
	}

	rest, ok := strings.CutPrefix(u.Path, TrashPrefix)
	if !ok {
		return "", errors.New("Destination must be inside the trashbin")
	}
	owner, tail, _ := strings.Cut(strings.Trim(rest, "/"), "/")
	if owner != user.Username {
		return "", errors.New("Destination must be inside your own trashbin")
	}
	collection, name := splitTrashPath(tail)
	if collection != restoreCollection {
		return "", fmt.Errorf("Destination must be in the %s collection", restoreCollection)
	}
	if name == "" {
		// The usual form: restore to wherever it came from.
		return "", nil
	}
	clean, err := fsx.CleanPath(name)
	if err != nil {
		return "", errors.New("Destination is not a valid path")
	}
	return clean, nil
}

// freeName returns target, or a variation on it that is not taken.
func (t *TrashHandler) freeName(r *http.Request, st *fsx.Storage, user store.User, target string) (string, error) {
	if _, err := st.Stat(target); err != nil {
		return target, nil
	}
	ext := path.Ext(target)
	base := strings.TrimSuffix(target, ext)
	for n := 2; n < 100; n++ {
		candidate := fmt.Sprintf("%s (restored %d)%s", base, n, ext)
		if _, err := st.Stat(candidate); err != nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name for %s", target)
}

// remove deletes an entry for good, or empties the whole trash.
func (t *TrashHandler) remove(w http.ResponseWriter, r *http.Request, user store.User, name string) {
	st, err := t.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		t.internalError(w, "open storage", err)
		return
	}

	if name == "" {
		entries, err := store.ListTrash(r.Context(), t.db, user.ID)
		if err != nil {
			t.internalError(w, "list trash", err)
			return
		}
		for _, e := range entries {
			t.forget(r, st, user, e)
		}
		t.log.Info("emptied the trash", "user", user.Username, "entries", len(entries))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	entry, err := store.TrashByName(r.Context(), t.db, user.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		t.internalError(w, "look up trash entry", err)
		return
	}
	t.forget(r, st, user, entry)
	w.WriteHeader(http.StatusNoContent)
}

// forget removes one entry from disk and from the index.
//
// The index row is dropped even if the file could not be removed, because an
// entry pointing at a file that is not there is worse than a stray file: a
// client would list it, and restoring it would fail.
func (t *TrashHandler) forget(r *http.Request, st *fsx.Storage, user store.User, e store.TrashEntry) {
	if err := st.RemoveFromTrash(e.Name); err != nil {
		t.log.Warn("could not remove a trashed file",
			"user", user.Username, "entry", e.Name, "error", err)
	}
	if err := store.RemoveTrashEntry(r.Context(), t.db, user.ID, e.Name); err != nil {
		t.log.Error("could not forget a trash entry",
			"user", user.Username, "entry", e.Name, "error", err)
	}
}

// mimeTypeOf guesses a content type from a name, for entries whose file is in
// the trash and not worth opening to find out.
func mimeTypeOf(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// trashHref builds the URL for a trashbin entry.
func trashHref(username, name string) string {
	p := TrashPrefix + username + "/" + trashCollection
	if name != "" {
		p += "/" + name
	} else {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}
