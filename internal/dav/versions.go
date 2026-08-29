package dav

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// VersionsPrefix is the WebDAV root for earlier copies of files.
const VersionsPrefix = "/remote.php/dav/versions/"

// The two collections under it: one lists a file's versions, the other is
// where a version is moved to put it back.
const (
	versionsCollection = "versions"
	versionsRestore    = "restore"
)

// VersionHandler serves the versions endpoints.
type VersionHandler struct {
	*Handler
}

// Versions returns a handler for earlier copies, sharing this handler's
// dependencies.
func (h *Handler) Versions() *VersionHandler { return &VersionHandler{Handler: h} }

// ServeHTTP dispatches a versions request.
func (v *VersionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := auth.MustUser(r.Context())
	if r.PathValue("user") != user.Username {
		http.NotFound(w, r)
		return
	}

	collection, rest := splitTrashPath(r.PathValue("path"))
	if collection != versionsCollection {
		http.NotFound(w, r)
		return
	}
	fileID, stamp, err := splitVersionPath(rest)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case "PROPFIND":
		v.list(w, r, user, fileID)
	case http.MethodGet, http.MethodHead:
		if stamp == 0 {
			http.NotFound(w, r)
			return
		}
		v.fetch(w, r, user, fileID, stamp)
	case "MOVE":
		if stamp == 0 {
			http.NotFound(w, r)
			return
		}
		v.restore(w, r, user, fileID, stamp)
	case http.MethodDelete:
		if stamp == 0 {
			http.NotFound(w, r)
			return
		}
		v.remove(w, r, user, fileID, stamp)
	case http.MethodOptions:
		w.Header().Set("DAV", ComplianceClasses)
		w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD, MOVE, DELETE")
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD, MOVE, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// splitVersionPath reads "{fileid}" or "{fileid}/{timestamp}".
func splitVersionPath(p string) (fileID, stamp int64, err error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return 0, 0, errors.New("no file named")
	}
	head, tail, _ := strings.Cut(p, "/")
	fileID, err = strconv.ParseInt(head, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if tail == "" {
		return fileID, 0, nil
	}
	stamp, err = strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return fileID, stamp, nil
}

// node resolves the file a request names, confined to the caller's account.
func (v *VersionHandler) node(r *http.Request, user store.User, fileID int64) (store.Node, bool) {
	node, err := store.NodeByID(r.Context(), v.db, user.ID, fileID)
	if err != nil {
		return store.Node{}, false
	}
	return node, true
}

// list answers a PROPFIND with a file's versions.
func (v *VersionHandler) list(w http.ResponseWriter, r *http.Request, user store.User, fileID int64) {
	node, ok := v.node(r, user, fileID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	names, allProps, err := parsePropfind(r.Body)
	if err != nil {
		http.Error(w, "malformed PROPFIND body", http.StatusBadRequest)
		return
	}
	if allProps || len(names) == 0 {
		names = versionPropNames
	}

	versions, err := store.VersionsOf(r.Context(), v.db, user.ID, node.ID)
	if err != nil {
		v.internalError(w, "list versions", err)
		return
	}

	ms := newMultistatus(w)
	ms.writeResponse(versionsHref(user.Username, fileID, 0), []prop{
		{Name: PropName{Space: NSDav, Local: "resourcetype"}, Value: "<d:collection/>"},
	}, nil)
	for _, ver := range versions {
		v.writeVersion(ms, user, fileID, node, ver, names)
	}
	if err := ms.close(); err != nil {
		v.log.Warn("could not finish the versions listing", "error", err)
	}
}

var versionPropNames = []PropName{
	{Space: NSDav, Local: "resourcetype"},
	{Space: NSDav, Local: "getcontentlength"},
	{Space: NSDav, Local: "getlastmodified"},
	{Space: NSDav, Local: "getcontenttype"},
	{Space: NSDav, Local: "getetag"},
	{Space: NSOwnCloud, Local: "size"},
	{Space: NSNextcloud, Local: "version-label"},
}

func (v *VersionHandler) writeVersion(ms *multistatus, user store.User, fileID int64,
	node store.Node, ver store.Version, names []PropName) {

	values := map[PropName]string{
		{Space: NSDav, Local: "resourcetype"}:     "",
		{Space: NSDav, Local: "getcontentlength"}: strconv.FormatInt(ver.Size, 10),
		{Space: NSDav, Local: "getlastmodified"}:  escapeText(ver.Timestamp.UTC().Format(http.TimeFormat)),
		{Space: NSDav, Local: "getcontenttype"}:   escapeText(mimeTypeOf(node.Name)),
		{Space: NSDav, Local: "getetag"}: escapeText(fmt.Sprintf(`"%d-%d"`,
			ver.NodeID, ver.Timestamp.Unix())),
		{Space: NSOwnCloud, Local: "size"}: strconv.FormatInt(ver.Size, 10),
		// Not a name somebody chose - Mirage has no way to set one - but a
		// client shows this beside each entry, and a date is more use than a
		// blank.
		{Space: NSNextcloud, Local: "version-label"}: escapeText(
			ver.Timestamp.Local().Format("2 Jan 2006, 15:04")),
	}

	var found []prop
	var missing []PropName
	for _, n := range names {
		if val, ok := values[n]; ok {
			found = append(found, prop{Name: n, Value: val})
			continue
		}
		missing = append(missing, n)
	}
	ms.writeResponse(versionsHref(user.Username, fileID, ver.Timestamp.Unix()), found, missing)
}

// fetch streams one version's contents.
func (v *VersionHandler) fetch(w http.ResponseWriter, r *http.Request, user store.User, fileID, stamp int64) {
	node, ok := v.node(r, user, fileID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ver, err := store.VersionAt(r.Context(), v.db, user.ID, node.ID, time.Unix(stamp, 0))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		v.internalError(w, "look up version", err)
		return
	}

	st, err := v.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		v.internalError(w, "open storage", err)
		return
	}
	f, err := st.OpenVersion(node.ID, stamp)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		v.internalError(w, "stat version", err)
		return
	}
	w.Header().Set("Content-Type", mimeTypeOf(node.Name))
	w.Header().Set("ETag", fmt.Sprintf(`"%d-%d"`, ver.NodeID, ver.Timestamp.Unix()))
	http.ServeContent(w, r, node.Name, info.ModTime(), f)
}

// restore puts a version back as the current contents of the file.
func (v *VersionHandler) restore(w http.ResponseWriter, r *http.Request, user store.User, fileID, stamp int64) {
	node, ok := v.node(r, user, fileID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := store.VersionAt(r.Context(), v.db, user.ID, node.ID, time.Unix(stamp, 0)); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := v.checkRestoreDestination(r, user); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	st, err := v.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		v.internalError(w, "open storage", err)
		return
	}

	// The current contents become a version of their own first, so that
	// restoring is itself undoable - somebody who restores the wrong one has
	// not lost anything.
	if err := v.keepVersion(r, st, user, node); err != nil {
		v.log.Error("could not keep the current contents before restoring a version",
			"user", user.Username, "path", node.Path, "error", err)
		http.Error(w, "could not restore that version", http.StatusInternalServerError)
		return
	}

	result, err := st.RestoreVersion(node.ID, stamp, node.Path, fsx.WriteOptions{})
	if err != nil {
		v.log.Warn("could not restore a version",
			"user", user.Username, "path", node.Path, "error", err)
		http.Error(w, "could not restore that version", http.StatusConflict)
		return
	}
	if _, err := v.updater.FileWritten(r.Context(), user, node.Path, result.Size, result.MTime); err != nil {
		v.internalError(w, "index restored version", err)
		return
	}

	v.log.Info("restored an earlier version",
		"user", user.Username, "path", node.Path, "version", time.Unix(stamp, 0))
	w.WriteHeader(http.StatusNoContent)
}

// checkRestoreDestination validates the Destination header.
//
// Its path decides nothing - a version is always restored over its own file -
// but it must at least name this account, or a client aimed at somebody else's
// version would be answered as though it had worked.
func (v *VersionHandler) checkRestoreDestination(r *http.Request, user store.User) error {
	raw := r.Header.Get("Destination")
	if raw == "" {
		return errors.New("MOVE requires a Destination")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("Destination is not a URL")
	}
	rest, ok := strings.CutPrefix(u.Path, VersionsPrefix)
	if !ok {
		return errors.New("Destination must be inside the versions endpoint")
	}
	owner, tail, _ := strings.Cut(strings.Trim(rest, "/"), "/")
	if owner != user.Username {
		return errors.New("Destination must be inside your own versions")
	}
	if collection, _ := splitTrashPath(tail); collection != versionsRestore {
		return fmt.Errorf("Destination must be in the %s collection", versionsRestore)
	}
	return nil
}

// remove discards one stored version.
func (v *VersionHandler) remove(w http.ResponseWriter, r *http.Request, user store.User, fileID, stamp int64) {
	node, ok := v.node(r, user, fileID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ver, err := store.VersionAt(r.Context(), v.db, user.ID, node.ID, time.Unix(stamp, 0))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		v.internalError(w, "look up version", err)
		return
	}

	st, err := v.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		v.internalError(w, "open storage", err)
		return
	}
	if err := st.RemoveVersion(node.ID, stamp); err != nil {
		v.log.Warn("could not remove a version file",
			"user", user.Username, "path", node.Path, "error", err)
	}
	if err := store.RemoveVersion(r.Context(), v.db, user.ID, ver.ID); err != nil {
		v.internalError(w, "forget version", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// versionsHref builds the URL for a file's versions, or one of them.
func versionsHref(username string, fileID, stamp int64) string {
	p := VersionsPrefix + username + "/" + versionsCollection + "/" + strconv.FormatInt(fileID, 10)
	if stamp != 0 {
		p += "/" + strconv.FormatInt(stamp, 10)
	} else {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}
