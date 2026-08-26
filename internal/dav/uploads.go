package dav

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/store"
)

// UploadsPrefix is the chunked upload endpoint.
const UploadsPrefix = "/remote.php/dav/uploads/"

// assembleMarker is the pseudo-resource a client MOVEs to trigger assembly.
const assembleMarker = ".file"

// UploadHandler implements chunked upload v2.
//
// A client creates a transfer directory, PUTs numbered chunks into it, then
// MOVEs the pseudo-file ".file" to the real destination to have them joined.
// The point is resumability: an interrupted upload resumes from the chunks
// already stored rather than starting over, which is what makes a large file
// over a slow link practical.
type UploadHandler struct {
	db         *store.DB
	storage    *fsx.Manager
	updater    *index.Updater
	log        *slog.Logger
	instanceID string
}

// NewUploadHandler builds the chunked upload handler.
func NewUploadHandler(db *store.DB, storage *fsx.Manager, updater *index.Updater,
	log *slog.Logger, instanceID string) *UploadHandler {
	return &UploadHandler{db: db, storage: storage, updater: updater, log: log, instanceID: instanceID}
}

func (h *UploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal := auth.MustUser(r.Context())

	if target := r.PathValue("user"); target != principal.Username {
		h.log.Warn("cross-account upload refused",
			"principal", principal.Username, "target", target, "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	transfer, chunk, err := splitUploadPath(r.PathValue("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "OPTIONS":
		w.Header().Set("Allow", "OPTIONS, PROPFIND, MKCOL, PUT, MOVE, DELETE")
		w.Header().Set("DAV", ComplianceClasses)
		w.WriteHeader(http.StatusOK)
	case "MKCOL":
		h.handleCreate(w, r, principal, transfer, chunk)
	case http.MethodPut:
		h.handleChunk(w, r, principal, transfer, chunk)
	case "MOVE":
		h.handleAssemble(w, r, principal, transfer, chunk)
	case http.MethodDelete:
		h.handleDiscard(w, r, principal, transfer, chunk)
	case "PROPFIND":
		h.handleList(w, r, principal, transfer)
	default:
		w.Header().Set("Allow", "OPTIONS, PROPFIND, MKCOL, PUT, MOVE, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// splitUploadPath separates the transfer id from an optional chunk name.
func splitUploadPath(raw string) (transfer, chunk string, err error) {
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", errors.New("missing transfer id")
	}
	if len(parts) > 2 {
		return "", "", errors.New("upload paths are at most two segments")
	}
	transfer = parts[0]
	if !fsx.ValidTransferID(transfer) {
		return "", "", errors.New("malformed transfer id")
	}
	if len(parts) == 2 {
		chunk = parts[1]
	}
	return transfer, chunk, nil
}

func (h *UploadHandler) handleCreate(w http.ResponseWriter, r *http.Request, user store.User, transfer, chunk string) {
	if chunk != "" {
		http.Error(w, "cannot create a collection inside a transfer", http.StatusMethodNotAllowed)
		return
	}
	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}
	if st.UploadExists(transfer) {
		http.Error(w, "transfer already exists", http.StatusMethodNotAllowed)
		return
	}
	if err := st.CreateUpload(transfer); err != nil {
		h.internalError(w, "create transfer", err)
		return
	}
	h.log.Debug("chunked upload started", "user", user.Username, "transfer", transfer)
	w.WriteHeader(http.StatusCreated)
}

func (h *UploadHandler) handleChunk(w http.ResponseWriter, r *http.Request, user store.User, transfer, chunk string) {
	if chunk == "" {
		http.Error(w, "missing chunk name", http.StatusBadRequest)
		return
	}
	if !fsx.ValidChunkName(chunk) {
		// Chunks are joined in numeric order, so a non-numeric name has no
		// defined position and must be refused rather than guessed at.
		http.Error(w, "chunk names must be numeric", http.StatusBadRequest)
		return
	}

	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}

	// Chunks occupy the user's disk while the transfer is in flight, so they
	// are counted against quota as they arrive rather than only at assembly.
	var maxBytes int64
	if user.Quota > 0 {
		usage, err := store.UserUsage(r.Context(), h.db, user.ID)
		if err != nil {
			h.internalError(w, "read usage", err)
			return
		}
		if maxBytes = user.Quota - usage; maxBytes <= 0 {
			http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
			return
		}
	}

	n, err := st.WriteChunk(transfer, chunk, r.Body, maxBytes)
	if err != nil {
		switch {
		case errors.Is(err, fsx.ErrNoSuchUpload):
			http.Error(w, "transfer does not exist; create it with MKCOL first", http.StatusNotFound)
		case errors.Is(err, fsx.ErrQuotaExceeded):
			http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
		case errors.Is(err, fsx.ErrInvalidPath):
			http.Error(w, "invalid chunk reference", http.StatusBadRequest)
		default:
			h.internalError(w, "store chunk", err)
		}
		return
	}
	h.log.Debug("chunk stored", "user", user.Username, "transfer", transfer, "chunk", chunk, "bytes", n)
	w.WriteHeader(http.StatusCreated)
}

func (h *UploadHandler) handleAssemble(w http.ResponseWriter, r *http.Request, user store.User, transfer, chunk string) {
	if chunk != assembleMarker {
		http.Error(w, "move "+assembleMarker+" to assemble a transfer", http.StatusMethodNotAllowed)
		return
	}
	dest, err := ParseDestination(r.Header.Get("Destination"), user.Username)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if fsx.IsRoot(dest) {
		http.Error(w, "cannot assemble onto the collection root", http.StatusBadRequest)
		return
	}

	existing, err := store.NodeByPath(r.Context(), h.db, user.ID, dest)
	switch {
	case err == nil && existing.IsDir:
		http.Error(w, "a directory already exists at the destination", http.StatusMethodNotAllowed)
		return
	case err != nil && !errors.Is(err, store.ErrNotFound):
		h.internalError(w, "look up destination", err)
		return
	}
	replacing := err == nil

	if _, err := store.NodeByPath(r.Context(), h.db, user.ID, parentOf(dest)); err != nil {
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
		remaining := user.Quota - usage
		if replacing {
			remaining += existing.Size
		}
		// The chunks already counted against usage, and assembly replaces them
		// with the finished file, so their combined size is available again.
		if chunks, err := st.ListChunks(transfer); err == nil {
			for _, c := range chunks {
				remaining += c.Size
			}
		}
		if remaining <= 0 {
			http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
			return
		}
		opts.MaxBytes = remaining
	}

	// The client may state the expected total; a mismatch means chunks were
	// lost, and joining them anyway would publish a silently truncated file.
	if declared := r.Header.Get("OC-Total-Length"); declared != "" {
		want, err := strconv.ParseInt(strings.TrimSpace(declared), 10, 64)
		if err != nil {
			http.Error(w, "malformed OC-Total-Length", http.StatusBadRequest)
			return
		}
		chunks, err := st.ListChunks(transfer)
		if err != nil {
			h.uploadError(w, err)
			return
		}
		var total int64
		for _, c := range chunks {
			total += c.Size
		}
		if total != want {
			h.log.Warn("refusing to assemble an incomplete transfer",
				"user", user.Username, "transfer", transfer, "have", total, "want", want)
			http.Error(w, fmt.Sprintf("incomplete transfer: have %d bytes, expected %d", total, want),
				http.StatusBadRequest)
			return
		}
	}

	res, err := st.AssembleUpload(transfer, dest, opts)
	if err != nil {
		h.uploadError(w, err)
		return
	}
	if !res.OwnershipApplied {
		h.log.Warn("could not set file ownership; the file is owned by the server process",
			"user", user.Username, "path", dest, "uid", user.UID, "gid", user.GID,
			"hint", "Mirage must run as root to chown files to another user")
	}

	node, err := h.updater.FileWritten(r.Context(), user, dest, res.Size, res.MTime)
	if err != nil {
		h.internalError(w, "index assembled file", err)
		return
	}
	h.log.Info("chunked upload assembled",
		"user", user.Username, "path", dest, "bytes", res.Size)

	w.Header().Set("ETag", `"`+node.ETag+`"`)
	w.Header().Set("OC-ETag", `"`+node.ETag+`"`)
	w.Header().Set("OC-FileId", fmt.Sprintf("%08d%s", node.ID, h.instanceID))
	if !opts.MTime.IsZero() {
		w.Header().Set("X-OC-MTime", "accepted")
	}
	if replacing {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *UploadHandler) handleDiscard(w http.ResponseWriter, r *http.Request, user store.User, transfer, chunk string) {
	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}
	if !st.UploadExists(transfer) {
		http.NotFound(w, r)
		return
	}
	if chunk != "" {
		// Dropping a single chunk lets a client re-send one it believes is bad.
		if !fsx.ValidChunkName(chunk) {
			http.Error(w, "chunk names must be numeric", http.StatusBadRequest)
			return
		}
		if err := st.Remove(fsx.UploadDir + "/" + transfer + "/" + chunk); err != nil {
			h.internalError(w, "discard chunk", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := st.DiscardUpload(transfer); err != nil {
		h.internalError(w, "discard transfer", err)
		return
	}
	h.log.Debug("chunked upload discarded", "user", user.Username, "transfer", transfer)
	w.WriteHeader(http.StatusNoContent)
}

// handleList reports which chunks the server already holds.
//
// This is what makes an interrupted upload resumable: the client asks what
// arrived and sends only the rest.
func (h *UploadHandler) handleList(w http.ResponseWriter, r *http.Request, user store.User, transfer string) {
	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.internalError(w, "open storage", err)
		return
	}
	chunks, err := st.ListChunks(transfer)
	if err != nil {
		h.uploadError(w, err)
		return
	}

	base := UploadsPrefix + user.Username + "/" + transfer
	ms := newMultistatus(w)
	ms.writeResponse(escapePath(base+"/"), []prop{
		{Name: propResourceType, Value: "<d:collection/>"},
	}, nil)
	for _, c := range chunks {
		ms.writeResponse(escapePath(base+"/"+c.Name), []prop{
			{Name: propGetContentLength, Value: strconv.FormatInt(c.Size, 10)},
			{Name: propGetLastModified, Value: escapeText(c.MTime.UTC().Format(http.TimeFormat))},
			{Name: propResourceType, Value: ""},
		}, nil)
	}
	if err := ms.close(); err != nil {
		h.log.Warn("could not finish upload listing", "user", user.Username, "error", err)
	}
}

func (h *UploadHandler) uploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fsx.ErrNoSuchUpload):
		http.Error(w, "no such transfer", http.StatusNotFound)
	case errors.Is(err, fsx.ErrQuotaExceeded):
		http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
	case errors.Is(err, fsx.ErrChecksumMismatch):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, fsx.ErrInvalidPath):
		http.Error(w, "invalid path", http.StatusBadRequest)
	default:
		h.internalError(w, "chunked upload", err)
	}
}

func (h *UploadHandler) internalError(w http.ResponseWriter, what string, err error) {
	h.log.Error("chunked upload failed", "operation", what, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// escapePath percent-encodes a URL path while leaving separators intact.
func escapePath(p string) string { return (&url.URL{Path: p}).EscapedPath() }
