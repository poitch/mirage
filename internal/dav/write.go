package dav

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
	"github.com/poitch/mirage/internal/store"
)

// handlePut stores an uploaded file.
func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request, user store.User, filePath string) {
	if fsx.IsRoot(filePath) {
		http.Error(w, "cannot write to the collection root", http.StatusMethodNotAllowed)
		return
	}

	existing, err := store.NodeByPath(r.Context(), h.db, user.ID, filePath)
	switch {
	case err == nil && existing.IsDir:
		w.Header().Set("Allow", h.allowedMethods())
		http.Error(w, "a directory already exists at this path", http.StatusMethodNotAllowed)
		return
	case err != nil && !errors.Is(err, store.ErrNotFound):
		h.internalError(w, "look up path", err)
		return
	}
	replacing := err == nil

	// RFC 4918: writing into a collection that does not exist is a conflict,
	// not a not-found. Clients respond by creating the parent with MKCOL.
	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, parentOf(filePath)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "parent collection does not exist", http.StatusConflict)
			return
		}
		h.internalError(w, "look up parent", err)
		return
	}

	opts := fsx.WriteOptions{Checksum: r.Header.Get("OC-Checksum")}
	if mtime, ok := parseOCMtime(r.Header.Get("X-OC-Mtime")); ok {
		opts.MTime = mtime
	}

	if user.Quota > 0 {
		usage, err := store.UserUsage(r.Context(), h.db, user.ID)
		if err != nil {
			h.internalError(w, "read usage", err)
			return
		}
		// Replacing a file frees whatever it currently occupies, so that space
		// counts as available for the new version.
		remaining := user.Quota - usage
		if replacing {
			remaining += existing.Size
		}
		if remaining <= 0 {
			http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
			return
		}
		opts.MaxBytes = remaining
	}

	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}

	// The earlier contents are copied aside before anything overwrites them.
	// Reported as an error rather than skipped: a save that silently loses the
	// only copy of what was there is the one failure versioning exists to
	// prevent.
	if replacing {
		if err := h.keepVersion(r, st, user, existing); err != nil {
			h.log.Error("could not keep a version of a file being overwritten",
				"user", user.Username, "path", filePath, "error", err)
			http.Error(w, "could not save the previous version of that file",
				http.StatusInternalServerError)
			return
		}
	}

	result, err := st.WriteFile(filePath, r.Body, opts)
	if err != nil {
		switch {
		case errors.Is(err, fsx.ErrQuotaExceeded):
			http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
		case errors.Is(err, fsx.ErrChecksumMismatch):
			h.log.Warn("upload failed checksum verification",
				"user", user.Username, "path", filePath, "error", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, fsx.ErrInvalidPath):
			http.Error(w, "invalid path", http.StatusBadRequest)
		default:
			h.internalError(w, "write file", err)
		}
		return
	}
	if !result.OwnershipApplied {
		// On a NAS this means the file is owned by whoever the server runs as
		// and will not be reachable over SMB, so it must not pass silently.
		h.warnOwnership(user, filePath)
	}

	node, err := h.updater.FileWritten(r.Context(), user, filePath, result.Size, result.MTime)
	if err != nil {
		h.internalError(w, "index written file", err)
		return
	}

	h.setEntityHeaders(w, node)
	if !opts.MTime.IsZero() {
		// Confirms the timestamp was honoured; clients check for this.
		w.Header().Set("X-OC-MTime", "accepted")
	}
	if replacing {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleMkcol creates a directory.
func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request, user store.User, dirPath string) {
	// RFC 4918 reserves a request body here for extended MKCOL, which Mirage
	// does not implement; rejecting it is better than ignoring what it asked.
	if r.ContentLength > 0 {
		http.Error(w, "request body is not supported on MKCOL", http.StatusUnsupportedMediaType)
		return
	}
	if fsx.IsRoot(dirPath) {
		w.Header().Set("Allow", h.allowedMethods())
		http.Error(w, "the collection root already exists", http.StatusMethodNotAllowed)
		return
	}

	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, dirPath); err == nil {
		w.Header().Set("Allow", h.allowedMethods())
		http.Error(w, "already exists", http.StatusMethodNotAllowed)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		h.internalError(w, "look up path", err)
		return
	}
	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, parentOf(dirPath)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "parent collection does not exist", http.StatusConflict)
			return
		}
		h.internalError(w, "look up parent", err)
		return
	}

	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}
	if err := st.Mkdir(dirPath); err != nil {
		if errors.Is(err, fsx.ErrInvalidPath) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		h.internalError(w, "create directory", err)
		return
	}

	node, err := h.updater.DirCreated(r.Context(), user, dirPath)
	if err != nil {
		h.internalError(w, "index new directory", err)
		return
	}
	h.setEntityHeaders(w, node)
	w.WriteHeader(http.StatusCreated)
}

// handleDelete removes a file or directory.
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request, user store.User, targetPath string) {
	if fsx.IsRoot(targetPath) {
		w.Header().Set("Allow", h.allowedMethods())
		http.Error(w, "cannot delete the collection root", http.StatusMethodNotAllowed)
		return
	}
	node, err := store.NodeByPath(r.Context(), h.db, user.ID, targetPath)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.internalError(w, "look up path", err)
		return
	}

	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}

	if h.trashRetention > 0 {
		if err := h.moveToTrash(r, st, user, node); err != nil {
			// Falling through to an outright delete would be the one behaviour
			// nobody wants from a trashbin, so the request fails instead and
			// the file stays where it is.
			h.log.Error("could not move a deleted file to the trash",
				"user", user.Username, "path", targetPath, "error", err)
			http.Error(w, "could not delete that file", http.StatusInternalServerError)
			return
		}
	} else if err := st.Remove(targetPath); err != nil {
		h.internalError(w, "delete", err)
		return
	}

	if err := h.updater.Removed(r.Context(), user, targetPath); err != nil {
		h.internalError(w, "index deletion", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// moveToTrash puts a deleted file aside instead of unlinking it.
//
// A directory goes in whole, in one rename, so deleting a folder of ten
// thousand photographs costs the same as deleting one file - and restoring it
// brings the lot back.
func (h *Handler) moveToTrash(r *http.Request, st *fsx.Storage, user store.User, node store.Node) error {
	name := fsx.TrashName(node.Path, time.Now())
	// Two files of the same name deleted from different folders in the same
	// second would collide, and the second would destroy the first.
	for n := 2; ; n++ {
		if _, err := store.TrashByName(r.Context(), h.db, user.ID, name); errors.Is(err, store.ErrNotFound) {
			break
		} else if err != nil {
			return err
		}
		if n > 100 {
			return fmt.Errorf("no free trash name for %s", node.Path)
		}
		name = fsx.TrashName(node.Path, time.Now()) + "-" + strconv.Itoa(n)
	}

	if err := st.MoveToTrash(node.Path, name); err != nil {
		return err
	}
	_, err := store.AddTrashEntry(r.Context(), h.db, user.ID, store.TrashEntry{
		Name:         name,
		OriginalPath: node.Path,
		DeletedAt:    time.Now(),
		Size:         node.Size,
		IsDir:        node.IsDir,
	})
	if err != nil {
		// The file is already aside. Putting it back is the only way to leave
		// things consistent, since an untracked entry is invisible and would
		// never expire.
		if restoreErr := st.RestoreFromTrash(name, node.Path); restoreErr != nil {
			h.log.Error("a file was moved to the trash but could not be recorded or put back",
				"user", user.Username, "path", node.Path, "entry", name, "error", restoreErr)
		}
		return err
	}
	h.log.Info("moved a deleted file to the trash",
		"user", user.Username, "path", node.Path, "entry", name)
	return nil
}

// keepVersion copies a file's current contents aside before it is overwritten.
//
// Files above the configured size are skipped rather than versioned, and that
// is reported as success: keeping a version of a five gigabyte video would cost
// more than the account's entire document history, and refusing the upload over
// it would be worse still.
func (h *Handler) keepVersion(r *http.Request, st *fsx.Storage, user store.User, node store.Node) error {
	if !h.versions.Enabled || node.IsDir {
		return nil
	}
	if h.versions.MaxFileSize > 0 && node.Size > h.versions.MaxFileSize {
		h.log.Debug("not keeping a version; the file is over the size limit",
			"user", user.Username, "path", node.Path, "size", node.Size)
		return nil
	}
	// An empty file has nothing worth keeping, and a save that only touches the
	// timestamp would otherwise fill the history with identical copies.
	if node.Size == 0 {
		return nil
	}

	// A version is addressed by whole seconds, because that is the resolution
	// clients use. The file's own modification time is the natural stamp - it
	// says when these contents were current - but two edits can share a second,
	// and collapsing them would silently discard one of the two. So a taken
	// stamp moves to the next free second rather than being skipped: slightly
	// inaccurate, and the alternative loses somebody's work.
	stamp := node.MTime.Truncate(time.Second)
	for range 60 {
		_, err := store.VersionAt(r.Context(), h.db, user.ID, node.ID, stamp)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return err
		}
		stamp = stamp.Add(time.Second)
	}

	size, err := st.SaveVersion(node.Path, node.ID, stamp.Unix())
	if err != nil {
		return err
	}
	if _, err := store.AddVersion(r.Context(), h.db, user.ID, store.Version{
		NodeID: node.ID, Path: node.Path, Timestamp: stamp, Size: size,
	}); err != nil {
		// The copy exists but nothing knows about it, which would leave it
		// occupying disk forever. Removing it is the only tidy outcome.
		if rmErr := st.RemoveVersion(node.ID, stamp.Unix()); rmErr != nil {
			h.log.Error("a version was written but could not be recorded or removed",
				"user", user.Username, "path", node.Path, "error", rmErr)
		}
		return err
	}

	h.trimVersions(r, st, user, node.ID)
	return nil
}

// trimVersions discards the oldest copies of a file beyond the limit.
//
// Best effort: failing to trim is not a reason to fail the save that triggered
// it, and the periodic sweep catches whatever is left.
func (h *Handler) trimVersions(r *http.Request, st *fsx.Storage, user store.User, nodeID int64) {
	if h.versions.MaxPerFile <= 0 {
		return
	}
	surplus, err := store.SurplusVersions(r.Context(), h.db, user.ID, nodeID, h.versions.MaxPerFile)
	if err != nil {
		h.log.Warn("could not read surplus versions", "user", user.Username, "error", err)
		return
	}
	for _, v := range surplus {
		if err := st.RemoveVersion(v.NodeID, v.Timestamp.Unix()); err != nil {
			h.log.Warn("could not remove a surplus version",
				"user", user.Username, "path", v.Path, "error", err)
		}
		if err := store.RemoveVersion(r.Context(), h.db, user.ID, v.ID); err != nil {
			h.log.Warn("could not forget a surplus version",
				"user", user.Username, "path", v.Path, "error", err)
		}
	}
}

// handleMoveCopy implements MOVE and COPY, which differ only in whether the
// source survives.
func (h *Handler) handleMoveCopy(w http.ResponseWriter, r *http.Request, user store.User, srcPath string, isMove bool) {
	dstPath, err := h.parseDestination(r, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isMove && fsx.IsRoot(srcPath) {
		http.Error(w, "cannot move the collection root", http.StatusMethodNotAllowed)
		return
	}
	if srcPath == dstPath {
		http.Error(w, "source and destination are the same", http.StatusForbidden)
		return
	}
	// Moving a directory inside itself would detach the subtree.
	if strings.HasPrefix(dstPath, srcPath+"/") {
		http.Error(w, "cannot move a collection into itself", http.StatusConflict)
		return
	}

	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, srcPath); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.internalError(w, "look up source", err)
		return
	}

	_, err = store.NodeByPath(r.Context(), h.db, user.ID, dstPath)
	dstExists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.internalError(w, "look up destination", err)
		return
	}
	// Overwrite defaults to T when the header is absent, per RFC 4918.
	if dstExists && strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "F") {
		http.Error(w, "destination exists and Overwrite is F", http.StatusPreconditionFailed)
		return
	}
	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, parentOf(dstPath)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "destination parent collection does not exist", http.StatusConflict)
			return
		}
		h.internalError(w, "look up destination parent", err)
		return
	}

	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}

	if isMove {
		// Remove whatever is at the destination first: rename over a non-empty
		// directory fails, and the client has already asked to replace it.
		if dstExists {
			if err := st.Remove(dstPath); err != nil {
				h.internalError(w, "clear destination", err)
				return
			}
		}
		if err := st.Rename(srcPath, dstPath); err != nil {
			h.internalError(w, "move", err)
			return
		}
		if err := h.updater.Moved(r.Context(), user, srcPath, dstPath); err != nil {
			h.internalError(w, "index move", err)
			return
		}
	} else {
		if dstExists {
			if err := st.Remove(dstPath); err != nil {
				h.internalError(w, "clear destination", err)
				return
			}
			if err := h.updater.Removed(r.Context(), user, dstPath); err != nil {
				h.internalError(w, "index destination removal", err)
				return
			}
		}
		if err := st.Copy(srcPath, dstPath); err != nil {
			if errors.Is(err, fsx.ErrInvalidPath) {
				http.Error(w, "invalid destination", http.StatusBadRequest)
				return
			}
			h.internalError(w, "copy", err)
			return
		}
		// A copied tree has many new entries, so it is indexed by rescanning
		// the destination rather than synthesising each one.
		if err := h.rescanPath(r, user, dstPath); err != nil {
			h.internalError(w, "index copy", err)
			return
		}
	}

	if dstExists {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleProppatch answers property writes.
//
// Mirage stores no settable properties: everything it reports is derived from
// the filesystem. Reporting each requested property as 403 is the honest
// answer, and clients treat it as "not supported here" rather than an error.
// The Nextcloud client sets modification times with the X-OC-Mtime header on
// upload, not through PROPPATCH, so nothing it needs depends on this.
func (h *Handler) handleProppatch(w http.ResponseWriter, r *http.Request, user store.User, targetPath string) {
	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, targetPath); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.internalError(w, "look up path", err)
		return
	}

	names, err := parseProppatch(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ms := newMultistatus(w)
	ms.writeStatusResponse(h.href(user.Username, targetPath, false), names, "HTTP/1.1 403 Forbidden")
	if err := ms.close(); err != nil {
		h.log.Warn("could not finish PROPPATCH response", "user", user.Username, "error", err)
	}
}

// parseDestination reads the Destination header and maps it onto a path inside
// the authenticated user's home.
func (h *Handler) parseDestination(r *http.Request, user store.User) (string, error) {
	return ParseDestination(r.Header.Get("Destination"), user.Username)
}

// ParseDestination maps a Destination header onto a path inside one user's
// home, refusing anything that points elsewhere.
//
// This is the one place a cross-account write could slip in, since the header
// carries a whole URL that the client chose.
func ParseDestination(raw, username string) (string, error) {
	if raw == "" {
		return "", errors.New("missing Destination header")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("malformed Destination header: %w", err)
	}

	// Destination is normally an absolute URI; only its path is meaningful, and
	// the host is deliberately not trusted to decide anything.
	p := u.Path
	var rest string
	switch {
	case strings.HasPrefix(p, FilesPrefix):
		rest = strings.TrimPrefix(p, FilesPrefix)
		owner, tail, _ := strings.Cut(rest, "/")
		if owner != username {
			return "", errors.New("destination is outside your files")
		}
		rest = tail
	case strings.HasPrefix(p, LegacyPrefix):
		rest = strings.TrimPrefix(p, LegacyPrefix)
	default:
		return "", errors.New("destination is not a WebDAV path on this server")
	}

	clean, err := fsx.CleanPath(rest)
	if err != nil {
		return "", errors.New("invalid destination path")
	}
	return clean, nil
}

// rescanPath reindexes a subtree after an operation that created many entries.
func (h *Handler) rescanPath(r *http.Request, user store.User, target string) error {
	return h.scanner.ScanPath(r.Context(), user, target)
}

// setEntityHeaders writes the identity headers clients read back after a write.
func (h *Handler) setEntityHeaders(w http.ResponseWriter, node store.Node) {
	w.Header().Set("ETag", `"`+node.ETag+`"`)
	w.Header().Set("OC-ETag", `"`+node.ETag+`"`)
	w.Header().Set("OC-FileId", fmt.Sprintf("%08d%s", node.ID, h.instanceID))
}

func (h *Handler) warnOwnership(user store.User, target string) {
	h.log.Warn("could not set file ownership; the file is owned by the server process",
		"user", user.Username, "path", target, "uid", user.UID, "gid", user.GID,
		"hint", "Mirage must run as root to chown files to another user")
}

// parseOCMtime reads the X-OC-Mtime header, a Unix timestamp in seconds.
func parseOCMtime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// parentOf returns the containing collection of a cleaned path.
func parentOf(p string) string {
	if fsx.IsRoot(p) {
		return fsx.RootPath
	}
	parent := path.Dir(p)
	if parent == "" || parent == "/" {
		return fsx.RootPath
	}
	return parent
}
