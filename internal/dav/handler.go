package dav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/store"
)

// FilesPrefix is the WebDAV root for a user's files.
const FilesPrefix = "/remote.php/dav/files/"

// LegacyPrefix is the pre-DAV root, still probed by desktop clients.
const LegacyPrefix = "/remote.php/webdav/"

// Handler serves the files WebDAV endpoint.
type Handler struct {
	db         *store.DB
	storage    *fsx.Manager
	updater    *index.Updater
	scanner    *index.Scanner
	log        *slog.Logger
	instanceID string
	// readOnly reflects what the server can currently honour. It drives both
	// the advertised oc:permissions and which methods are allowed, so the two
	// can never disagree.
	readOnly bool
}

// NewHandler builds the files handler.
func NewHandler(db *store.DB, storage *fsx.Manager, updater *index.Updater,
	scanner *index.Scanner, log *slog.Logger, instanceID string, readOnly bool) *Handler {
	return &Handler{
		db: db, storage: storage, updater: updater, scanner: scanner,
		log: log, instanceID: instanceID, readOnly: readOnly,
	}
}

// allowedMethods is the Allow header, and the authority on what is accepted.
func (h *Handler) allowedMethods() string {
	if h.readOnly {
		return "OPTIONS, HEAD, GET, PROPFIND"
	}
	return "OPTIONS, HEAD, GET, PUT, DELETE, MKCOL, COPY, MOVE, PROPFIND, PROPPATCH"
}

// ServeSearch answers a WebDAV SEARCH, which clients send to the DAV root
// rather than to a collection.
func (h *Handler) ServeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "SEARCH" {
		w.Header().Set("Allow", "SEARCH")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.handleSearch(w, r)
}

// ServeHTTP dispatches a request to /remote.php/dav/files/{user}/..., where
// the account is named in the path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal := auth.MustUser(r.Context())
	h.serve(w, r, principal, r.PathValue("user"), r.PathValue("path"))
}

// ServeLegacy dispatches a request to /remote.php/webdav/..., the older root
// that carries no username and always refers to the authenticated account.
// Desktop clients still probe it during setup.
func (h *Handler) ServeLegacy(w http.ResponseWriter, r *http.Request) {
	principal := auth.MustUser(r.Context())
	h.serve(w, r, principal, principal.Username, r.PathValue("path"))
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, principal store.User, targetUser, rawPath string) {
	if targetUser != principal.Username {
		// Answering 404 rather than 403 avoids confirming whether the other
		// account exists. The warning gives an operator what they need to
		// diagnose a genuine misconfiguration.
		h.log.Warn("cross-account request refused",
			"principal", principal.Username, "target", targetUser, "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	cleanPath, err := fsx.CleanPath(rawPath)
	if err != nil {
		h.log.Warn("rejected malformed path",
			"user", principal.Username, "path", rawPath, "error", err)
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "OPTIONS":
		h.handleOptions(w)
	case "PROPFIND":
		h.handlePropfind(w, r, principal, cleanPath)
	case http.MethodGet, http.MethodHead:
		h.handleGet(w, r, principal, cleanPath)
	case http.MethodPut:
		h.ifWritable(w, func() { h.handlePut(w, r, principal, cleanPath) })
	case http.MethodDelete:
		h.ifWritable(w, func() { h.handleDelete(w, r, principal, cleanPath) })
	case "MKCOL":
		h.ifWritable(w, func() { h.handleMkcol(w, r, principal, cleanPath) })
	case "MOVE":
		h.ifWritable(w, func() { h.handleMoveCopy(w, r, principal, cleanPath, true) })
	case "COPY":
		h.ifWritable(w, func() { h.handleMoveCopy(w, r, principal, cleanPath, false) })
	case "PROPPATCH":
		h.ifWritable(w, func() { h.handleProppatch(w, r, principal, cleanPath) })
	default:
		w.Header().Set("Allow", h.allowedMethods())
		w.Header().Set("DAV", ComplianceClasses)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ifWritable runs fn unless the server is in read-only mode, in which case it
// answers 405. Routing writes through one place keeps the accepted methods and
// the advertised oc:permissions from drifting apart.
func (h *Handler) ifWritable(w http.ResponseWriter, fn func()) {
	if h.readOnly {
		w.Header().Set("Allow", h.allowedMethods())
		w.Header().Set("DAV", ComplianceClasses)
		http.Error(w, "server is read-only", http.StatusMethodNotAllowed)
		return
	}
	fn()
}

func (h *Handler) handleOptions(w http.ResponseWriter) {
	w.Header().Set("Allow", h.allowedMethods())
	// The DAV header is what tells a client this is a WebDAV server rather than
	// a plain HTTP file host.
	w.Header().Set("DAV", ComplianceClasses)
	w.Header().Set("MS-Author-Via", "DAV")
	w.WriteHeader(http.StatusOK)
}

// parseDepth reads the Depth header.
//
// RFC 4918 makes infinity the default, but a missing Depth on a large tree
// would then walk everything and build an enormous response. Every real client
// sends the header, so defaulting to 1 costs nothing and removes the footgun.
func parseDepth(r *http.Request) (depth int, ok bool) {
	switch strings.TrimSpace(r.Header.Get("Depth")) {
	case "0":
		return 0, true
	case "", "1":
		return 1, true
	case "infinity":
		return -1, true
	default:
		return 0, false
	}
}

func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request, user store.User, path string) {
	depth, ok := parseDepth(r)
	if !ok {
		http.Error(w, "invalid Depth header", http.StatusBadRequest)
		return
	}

	names, allProps, err := parsePropfind(r.Body)
	if err != nil {
		h.log.Warn("malformed PROPFIND", "user", user.Username, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if allProps {
		names = allPropNames
	}

	node, err := store.NodeByPath(r.Context(), h.db, user.ID, path)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.internalError(w, "look up path", err)
		return
	}

	usage, err := store.UserUsage(r.Context(), h.db, user.ID)
	if err != nil {
		h.internalError(w, "read usage", err)
		return
	}

	var children []store.Node
	switch {
	case depth == 1 && node.IsDir:
		if children, err = store.ChildNodes(r.Context(), h.db, node.ID); err != nil {
			h.internalError(w, "list children", err)
			return
		}
	case depth == -1 && node.IsDir:
		if children, err = store.SubtreeNodes(r.Context(), h.db, user.ID, path); err != nil {
			h.internalError(w, "list subtree", err)
			return
		}
	}

	ms := newMultistatus(w)
	h.writeNode(ms, user, node, usage, names)
	for _, child := range children {
		h.writeNode(ms, user, child, usage, names)
	}
	if err := ms.close(); err != nil {
		// The response is already partly written, so this can only be logged.
		h.log.Warn("could not finish PROPFIND response", "user", user.Username, "error", err)
	}
}

func (h *Handler) writeNode(ms *multistatus, user store.User, node store.Node, usage int64, names []PropName) {
	rc := resourceContext{
		node: node, user: user,
		instanceID: h.instanceID, usage: usage, readOnly: h.readOnly,
	}
	found, missing := rc.resolveAll(names)
	ms.writeResponse(h.href(user.Username, node.Path, node.IsDir), found, missing)
}

// href builds the URL path for an indexed entry.
//
// Directories carry a trailing slash: clients use it to tell a collection from
// a file before they have parsed the resourcetype property.
func (h *Handler) href(username, path string, isDir bool) string {
	p := FilesPrefix + username
	if path != "." && path != "" {
		p += "/" + path
	}
	if isDir && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	// EscapedPath encodes each segment while leaving separators intact.
	return (&url.URL{Path: p}).EscapedPath()
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, user store.User, path string) {
	node, err := store.NodeByPath(r.Context(), h.db, user.ID, path)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.internalError(w, "look up path", err)
		return
	}
	if node.IsDir {
		w.Header().Set("Allow", h.allowedMethods())
		http.Error(w, "cannot download a directory", http.StatusMethodNotAllowed)
		return
	}

	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}
	f, err := st.Open(path)
	if err != nil {
		// The index said this file exists but the filesystem disagrees, which
		// means it was removed out of band since the last scan. That is an
		// expected race, not a server fault.
		h.log.Info("indexed file is missing on disk",
			"user", user.Username, "path", path, "error", err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		h.internalError(w, "stat file", err)
		return
	}

	// The ETag is recomputed from the file being served rather than taken from
	// the index.
	//
	// They can disagree. A file rewritten in place under the same name leaves
	// its directory's timestamp untouched, so a quick pass cannot see it and
	// the index stays behind until the next full rescan. Serving the index's
	// ETag alongside the real file's bytes would then answer a conditional
	// request with 304 Not Modified for content that had in fact changed - so a
	// client keeping a local copy would never refresh it.
	//
	// The stat needed for this has already happened, so the check is free, and
	// a disagreement is worth correcting for everyone rather than only for the
	// client that happened to ask.
	etag := index.FileETag(info.Size(), info.ModTime())
	if etag != node.ETag {
		h.log.Info("file changed since it was indexed; correcting the index",
			"user", user.Username, "path", path)
		h.reindexInBackground(user, path, info.Size(), info.ModTime())
	}

	// Clients read these back on upload and download to confirm identity.
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("OC-ETag", `"`+etag+`"`)
	w.Header().Set("OC-FileId", fmt.Sprintf("%08d%s", node.ID, h.instanceID))
	if node.ContentType != "" {
		w.Header().Set("Content-Type", node.ContentType)
	}

	// ServeContent handles Range, If-Range and If-None-Match, which is what
	// makes an interrupted download resumable.
	http.ServeContent(w, r, node.Name, info.ModTime(), f)
}

// reindexInBackground corrects one file's index entry without delaying the
// response that noticed the problem.
//
// Detached from the request context on purpose: the client has what it asked
// for and may disconnect immediately, but the correction is for every other
// client too and should not be abandoned with it.
func (h *Handler) reindexInBackground(user store.User, path string, size int64, mtime time.Time) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := h.updater.FileWritten(ctx, user, path, size, mtime); err != nil {
			h.log.Warn("could not correct the index for a file that changed on disk",
				"user", user.Username, "path", path, "error", err)
		}
	}()
}

func (h *Handler) internalError(w http.ResponseWriter, what string, err error) {
	h.log.Error("webdav request failed", "operation", what, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
