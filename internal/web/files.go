package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/preview"
	"github.com/poitch/mirage/internal/store"
)

// searchLimit bounds what a search returns. A person scanning a list has no use
// for more, and the query walks the account.
const searchLimit = 200

// search finds files by name.
//
// By name only, deliberately: this is the box somebody types a half-remembered
// filename into. The clients have a richer search for everything else.
func (s *Site) search(w http.ResponseWriter, r *http.Request, sess *session) {
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	if term == "" {
		http.Redirect(w, r, rootPath, http.StatusSeeOther)
		return
	}

	matches, err := store.SearchNodes(r.Context(), s.db, sess.userID, ".",
		store.SubstringPattern(term), "", searchLimit)
	if err != nil {
		s.internalError(w, "search", err)
		return
	}

	entries := make([]entry, 0, len(matches))
	for _, n := range matches {
		e := s.entryFor(sess, n)
		// A result out of context is just a filename; the folder is what tells
		// two files of the same name apart.
		e.Where = "in " + displayFolder(n.Path)
		entries = append(entries, e)
	}

	s.render(w, "browse.html", http.StatusOK, pageData{
		Title:     "Search",
		Username:  sess.username,
		CSRF:      sess.csrf,
		Query:     term,
		Searching: true,
		Entries:   entries,
	})
}

// entryFor renders one indexed node as a row.
func (s *Site) entryFor(sess *session, n store.Node) entry {
	e := entry{
		Name:     n.Name,
		IsDir:    n.IsDir,
		Size:     formatBytes(n.Size),
		Modified: n.MTime.Local().Format("2 Jan 2006, 15:04"),
	}
	if n.IsDir {
		e.URL = browseURL(n.Path, "")
		return e
	}
	e.URL = downloadURL(n.Path)
	if s.previews != nil && preview.Supported(n.Name) {
		e.ThumbURL = thumbURL(n.ID)
	}
	if s.versionsEnabled {
		e.VersionsURL = versionsPageURL(n.ID)
	}
	return e
}

// thumbnail serves a preview for the browser session.
//
// The sync endpoint cannot be used from here: it authenticates as a sync
// account and this page has a session instead.
func (s *Site) thumbnail(w http.ResponseWriter, r *http.Request, sess *session) {
	if s.previews == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	user, err := s.db.UserByID(r.Context(), sess.userID)
	if err != nil {
		s.internalError(w, "look up account", err)
		return
	}
	// Handed to the preview handler with the account already resolved, so the
	// same confinement applies as on the sync endpoint.
	s.previews.ServeFor(w, r, user, id, previewThumbSize)
}

// previewThumbSize is what a row in a listing shows. Small enough that the
// camera's own embedded thumbnail usually answers it without the photograph
// being decoded at all.
const previewThumbSize = 128

// trash lists an account's deleted files.
func (s *Site) trash(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.trashEnabled {
		http.NotFound(w, r)
		return
	}
	entries, err := store.ListTrash(r.Context(), s.db, sess.userID)
	if err != nil {
		s.internalError(w, "list deleted files", err)
		return
	}

	rows := make([]entry, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, entry{
			Name:     path.Base(e.OriginalPath),
			IsDir:    e.IsDir,
			Size:     formatBytes(e.Size),
			Modified: e.DeletedAt.Local().Format("2 Jan 2006, 15:04"),
			Where:    displayFolder(e.OriginalPath),
			Token:    e.Name,
		})
	}

	s.render(w, "trash.html", http.StatusOK, pageData{
		Title:    "Deleted files",
		Username: sess.username,
		CSRF:     sess.csrf,
		Notice:   r.URL.Query().Get("notice"),
		Entries:  rows,
	})
}

// restoreTrash puts a deleted file back.
func (s *Site) restoreTrash(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.trashEnabled {
		http.NotFound(w, r)
		return
	}
	user, entry, st, ok := s.trashTarget(w, r, sess)
	if !ok {
		return
	}

	// Restoring never overwrites: if something is at the original path now, the
	// file comes back beside it.
	target, err := freePath(st, entry.OriginalPath)
	if err != nil {
		s.internalError(w, "choose a restore path", err)
		return
	}
	if err := st.RestoreFromTrash(entry.Name, target); err != nil {
		s.log.Warn("could not restore a deleted file",
			"user", user.Username, "entry", entry.Name, "error", err)
		s.redirect(w, r, "/web/trash", "That file could not be restored.")
		return
	}
	if err := store.RemoveTrashEntry(r.Context(), s.db, user.ID, entry.Name); err != nil {
		s.internalError(w, "forget trash entry", err)
		return
	}
	// Indexed now, or the restored file would be invisible to every client
	// until the next scan and the restore would look like it failed.
	if err := s.scanner.ScanPath(r.Context(), user, target); err != nil {
		s.log.Warn("restored a file but could not index it",
			"user", user.Username, "path", target, "error", err)
	}

	s.log.Info("restored a deleted file from the web", "user", user.Username, "path", target)
	s.redirect(w, r, "/web/trash", "Restored to "+target+".")
}

// deleteTrash removes a deleted file for good.
func (s *Site) deleteTrash(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.trashEnabled {
		http.NotFound(w, r)
		return
	}
	user, entry, st, ok := s.trashTarget(w, r, sess)
	if !ok {
		return
	}
	if err := st.RemoveFromTrash(entry.Name); err != nil {
		s.log.Warn("could not remove a trashed file",
			"user", user.Username, "entry", entry.Name, "error", err)
	}
	// Forgotten either way: an entry whose file is gone would still be listed
	// and would fail to restore.
	if err := store.RemoveTrashEntry(r.Context(), s.db, user.ID, entry.Name); err != nil {
		s.internalError(w, "forget trash entry", err)
		return
	}
	s.redirect(w, r, "/web/trash", "Removed for good.")
}

// trashTarget resolves the entry a form names, confined to the account.
func (s *Site) trashTarget(w http.ResponseWriter, r *http.Request, sess *session) (
	store.User, store.TrashEntry, *fsx.Storage, bool) {

	user, err := s.db.UserByID(r.Context(), sess.userID)
	if err != nil {
		s.internalError(w, "look up account", err)
		return store.User{}, store.TrashEntry{}, nil, false
	}
	// Looked up against this account, so a name from anywhere else is simply
	// not found.
	entry, err := store.TrashByName(r.Context(), s.db, user.ID, r.PostFormValue("entry"))
	if errors.Is(err, store.ErrNotFound) {
		s.redirect(w, r, "/web/trash", "That file is no longer in the trash.")
		return store.User{}, store.TrashEntry{}, nil, false
	} else if err != nil {
		s.internalError(w, "look up trash entry", err)
		return store.User{}, store.TrashEntry{}, nil, false
	}
	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		s.internalError(w, "open storage", err)
		return store.User{}, store.TrashEntry{}, nil, false
	}
	return user, entry, st, true
}

// versionsPage lists a file's earlier copies.
func (s *Site) versionsPage(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.versionsEnabled {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	node, err := store.NodeByID(r.Context(), s.db, sess.userID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	versions, err := store.VersionsOf(r.Context(), s.db, sess.userID, node.ID)
	if err != nil {
		s.internalError(w, "list versions", err)
		return
	}
	rows := make([]entry, 0, len(versions))
	for _, v := range versions {
		rows = append(rows, entry{
			Modified: v.Timestamp.Local().Format("2 Jan 2006, 15:04"),
			Size:     formatBytes(v.Size),
			URL:      versionDownloadURL(node.ID, v.Timestamp.Unix()),
			Token:    strconv.FormatInt(v.Timestamp.Unix(), 10),
		})
	}

	s.render(w, "versions.html", http.StatusOK, pageData{
		Title:           node.Name,
		Username:        sess.username,
		CSRF:            sess.csrf,
		Notice:          r.URL.Query().Get("notice"),
		FileID:          node.ID,
		Entries:         rows,
		ParentURL:       browseURL(path.Dir(node.Path), node.Name),
		ParentName:      displayFolder(node.Path),
		DownloadURL:     downloadURL(node.Path),
		CurrentSize:     formatBytes(node.Size),
		CurrentModified: node.MTime.Local().Format("2 Jan 2006, 15:04"),
	})
}

// downloadVersion streams one earlier copy.
func (s *Site) downloadVersion(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.versionsEnabled {
		http.NotFound(w, r)
		return
	}
	user, node, ver, st, ok := s.versionTarget(w, r, sess,
		r.PathValue("id"), r.PathValue("stamp"))
	if !ok {
		return
	}
	_ = user

	f, err := st.OpenVersion(node.ID, ver.Timestamp.Unix())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.internalError(w, "stat version", err)
		return
	}

	// An attachment, never inline, for the same reason live files are: an HTML
	// file rendered here would run as a page on this origin.
	name := versionFileName(node.Name, ver.Timestamp)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// restoreVersion makes an earlier copy the current contents.
func (s *Site) restoreVersion(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.versionsEnabled {
		http.NotFound(w, r)
		return
	}
	user, node, ver, st, ok := s.versionTarget(w, r, sess,
		r.PostFormValue("file"), r.PostFormValue("version"))
	if !ok {
		return
	}

	// What is there now becomes a version first, so restoring the wrong one
	// costs nothing.
	if err := s.keeper.Keep(r.Context(), st, user, node); err != nil {
		s.log.Error("could not keep the current contents before restoring",
			"user", user.Username, "path", node.Path, "error", err)
		s.redirect(w, r, versionsPageURL(node.ID), "That version could not be restored.")
		return
	}

	result, err := st.RestoreVersion(node.ID, ver.Timestamp.Unix(), node.Path, fsx.WriteOptions{})
	if err != nil {
		s.log.Warn("could not restore a version",
			"user", user.Username, "path", node.Path, "error", err)
		s.redirect(w, r, versionsPageURL(node.ID), "That version could not be restored.")
		return
	}
	if _, err := s.updater.FileWritten(r.Context(), user, node.Path, result.Size, result.MTime); err != nil {
		s.internalError(w, "index restored version", err)
		return
	}

	s.log.Info("restored an earlier version from the web",
		"user", user.Username, "path", node.Path, "version", ver.Timestamp)
	s.redirect(w, r, versionsPageURL(node.ID),
		"Restored the version from "+ver.Timestamp.Local().Format("2 Jan 2006, 15:04")+".")
}

// versionTarget resolves the file and version a request names.
func (s *Site) versionTarget(w http.ResponseWriter, r *http.Request, sess *session, rawID, rawStamp string) (
	store.User, store.Node, store.Version, *fsx.Storage, bool) {

	var zero struct {
		u store.User
		n store.Node
		v store.Version
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return zero.u, zero.n, zero.v, nil, false
	}
	stamp, err := strconv.ParseInt(rawStamp, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return zero.u, zero.n, zero.v, nil, false
	}
	user, err := s.db.UserByID(r.Context(), sess.userID)
	if err != nil {
		s.internalError(w, "look up account", err)
		return zero.u, zero.n, zero.v, nil, false
	}
	// Both lookups are scoped to the account, so another account's file id or
	// version reports as missing rather than being refused.
	node, err := store.NodeByID(r.Context(), s.db, user.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return zero.u, zero.n, zero.v, nil, false
	}
	ver, err := store.VersionAt(r.Context(), s.db, user.ID, node.ID, time.Unix(stamp, 0))
	if err != nil {
		http.NotFound(w, r)
		return zero.u, zero.n, zero.v, nil, false
	}
	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		s.internalError(w, "open storage", err)
		return zero.u, zero.n, zero.v, nil, false
	}
	return user, node, ver, st, true
}

// freePath returns target, or a variation on it that is not taken.
func freePath(st *fsx.Storage, target string) (string, error) {
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

// versionFileName names a downloaded version so two of them do not land in the
// download folder under the same name.
func versionFileName(name string, at time.Time) string {
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + " (" + at.Local().Format("2006-01-02 1504") + ")" + ext
}

// displayFolder renders the folder part of a path for a person to read.
func displayFolder(p string) string {
	dir := path.Dir(p)
	if dir == "." || dir == "/" || dir == "" {
		return "the top level"
	}
	return dir
}

// redirect sends the browser back to a page with something to say on it.
func (s *Site) redirect(w http.ResponseWriter, r *http.Request, to, notice string) {
	http.Redirect(w, r, to+"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

func thumbURL(fileID int64) string {
	return "/web/thumb/" + strconv.FormatInt(fileID, 10)
}

func versionsPageURL(fileID int64) string {
	return "/web/versions/" + strconv.FormatInt(fileID, 10)
}

func versionDownloadURL(fileID, stamp int64) string {
	return fmt.Sprintf("/web/versions/%d/%d", fileID, stamp)
}
