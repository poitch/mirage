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
	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, targetPath); err != nil {
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
	if err := st.Remove(targetPath); err != nil {
		h.internalError(w, "delete", err)
		return
	}
	if err := h.updater.Removed(r.Context(), user, targetPath); err != nil {
		h.internalError(w, "index deletion", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	raw := r.Header.Get("Destination")
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
		if owner != user.Username {
			// The one place a cross-account write could sneak in.
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
