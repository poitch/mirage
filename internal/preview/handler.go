package preview

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// Path is where clients ask for previews.
const Path = "/index.php/core/preview"

// maxAge is how long a client may keep a preview. Long, because the URL carries
// the file id and the size, and a changed file gets a different ETag.
const maxAge = 7 * 24 * time.Hour

// Handler serves previews.
type Handler struct {
	db      *store.DB
	storage *fsx.Manager
	cache   *Cache
	log     *slog.Logger
}

// NewHandler builds the preview endpoint.
func NewHandler(db *store.DB, storage *fsx.Manager, cache *Cache, log *slog.Logger) *Handler {
	return &Handler{db: db, storage: storage, cache: cache, log: log}
}

// ServeHTTP answers a preview request.
//
// A file with no preview is answered 404 rather than with a placeholder. That
// is what the clients expect: they draw their own icon for the file type, which
// looks better than anything this could invent and costs no transfer.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := auth.MustUser(r.Context())

	node, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	if node.IsDir || !Supported(node.Name) {
		http.NotFound(w, r)
		return
	}

	size := Bucket(requestedSize(r))
	key := Key(user.ID, node.ID, node.ETag, size)
	etag := `"` + key[:24] + `"`

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(maxAge.Seconds())))
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if data := h.cache.Get(key); data != nil {
		h.write(w, r, data)
		return
	}

	// One slot at a time: making a preview reads a whole photograph, and a
	// phone scrolling a gallery asks for dozens at once.
	release, ok := h.cache.Acquire(r.Context().Done())
	if !ok {
		// The client went away while queued. Nothing to answer.
		return
	}
	defer release()

	// Checked again: something else may have made it while this request waited.
	if data := h.cache.Get(key); data != nil {
		h.write(w, r, data)
		return
	}

	st, err := h.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		h.log.Error("could not open storage for a preview", "user", user.Username, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	f, err := st.Open(node.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	start := time.Now()
	result, err := Generate(f, node.Name, size)
	if errors.Is(err, ErrUnsupported) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		h.log.Warn("could not make a preview",
			"user", user.Username, "path", node.Path, "error", err)
		http.NotFound(w, r)
		return
	}

	h.log.Debug("made a preview",
		"user", user.Username, "path", node.Path, "size", size,
		"from_thumbnail", result.FromThumbnail, "took", time.Since(start))

	h.cache.Put(key, result.Data)
	h.write(w, r, result.Data)
}

// resolve finds the file a request is asking about.
//
// Clients address it either by index id or by path, and both must be confined
// to the caller's own account: the id is the only input on one of those paths,
// so nothing else would confine it.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, user store.User) (store.Node, bool) {
	q := r.URL.Query()

	if raw := q.Get("fileId"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return store.Node{}, false
		}
		node, err := store.NodeByID(r.Context(), h.db, user.ID, id)
		if err != nil {
			// Another account's file is reported missing rather than refused,
			// so this cannot be used to find out what exists.
			http.NotFound(w, r)
			return store.Node{}, false
		}
		return node, true
	}

	raw := q.Get("file")
	if raw == "" {
		raw = q.Get("path")
	}
	clean, err := fsx.CleanPath(raw)
	if err != nil {
		http.NotFound(w, r)
		return store.Node{}, false
	}
	node, err := store.NodeByPath(r.Context(), h.db, user.ID, clean)
	if err != nil {
		http.NotFound(w, r)
		return store.Node{}, false
	}
	return node, true
}

// requestedSize reads the dimensions a client asked for.
//
// Clients send x and y separately and mean "a box this big". The larger is
// taken, because the preview is fitted inside the box either way and taking the
// smaller would make it too small for the other axis.
func requestedSize(r *http.Request) int {
	q := r.URL.Query()
	x, _ := strconv.Atoi(q.Get("x"))
	y, _ := strconv.Atoi(q.Get("y"))
	if n := max(x, y); n > 0 {
		return n
	}
	return 256
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	w.Write(data)
}
