// Package push implements the notify_push protocol, which tells clients that
// something changed instead of making them ask.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/store"
)

const (
	// authTimeout bounds how long a connection may sit unauthenticated.
	authTimeout = 15 * time.Second
	// keepalive is how often a ping goes out. Home routers and reverse proxies
	// drop idle connections, and a dropped push connection silently degrades
	// sync back to polling.
	keepalive = 45 * time.Second
	// sendBuffer is how many notifications may queue for one connection before
	// it is considered stuck.
	sendBuffer = 32
	// maxMessageSize caps what a client may send. The protocol's messages are a
	// username, a password and short commands.
	maxMessageSize = 4096
)

// Protocol messages.
const (
	msgAuthenticated = "authenticated"
	msgNotifyFile    = "notify_file"
	msgNotifyFileID  = "notify_file_id"
	cmdListenFileID  = "listen notify_file_id"
	msgInvalidCreds  = "err: Invalid credentials"
)

// Hub tracks connected clients and fans changes out to them.
//
// It holds no queue and no history: a notification is a hint to synchronise,
// not a record of what happened. A client that misses one finds the change on
// its next sync anyway, which is why dropping a message for a stuck connection
// is safe.
type Hub struct {
	auth *auth.Authenticator
	db   *store.DB
	log  *slog.Logger

	mu      sync.RWMutex
	clients map[int64]map[*client]struct{}

	preAuth *tokenStore
}

// NewHub builds a Hub.
func NewHub(a *auth.Authenticator, db *store.DB, log *slog.Logger) *Hub {
	return &Hub{
		auth:    a,
		db:      db,
		log:     log,
		clients: make(map[int64]map[*client]struct{}),
		preAuth: newTokenStore(),
	}
}

// client is one connected websocket.
type client struct {
	conn   *websocket.Conn
	userID int64
	send   chan notification
	// wantFileIDs is set when the client asked for changed file ids rather than
	// a bare "something changed". It is written by the reader goroutine and
	// read by the writer, so it is atomic: "only one goroutine writes it" is
	// not what makes a concurrent read safe.
	wantFileIDs atomic.Bool
}

type notification struct {
	fileIDs []int64
}

// FileChanged tells a user's connected clients that their files changed.
//
// It never blocks: the index holds a lock while it calls this, and a slow
// client must not be able to stall writes for everyone.
func (h *Hub) FileChanged(userID int64, fileIDs []int64) {
	h.mu.RLock()
	conns := make([]*client, 0, len(h.clients[userID]))
	for c := range h.clients[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		select {
		case c.send <- notification{fileIDs: fileIDs}:
		default:
			// The client is not keeping up. Dropping the hint costs it nothing
			// but latency, since its next poll finds the change regardless.
			h.log.Debug("dropped a push notification for a slow client", "user_id", userID)
		}
	}
}

// Connected reports how many clients are currently listening, for diagnostics.
func (h *Hub) Connected() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var n int
	for _, set := range h.clients {
		n += len(set)
	}
	return n
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[c.userID] == nil {
		h.clients[c.userID] = make(map[*client]struct{})
	}
	h.clients[c.userID][c] = struct{}{}
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.clients[c.userID]
	delete(set, c)
	if len(set) == 0 {
		delete(h.clients, c.userID)
	}
}

// ServeWS handles /push/ws.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The check is skipped because this endpoint carries no cookie session:
		// every connection authenticates itself with credentials after the
		// handshake, so an origin cannot speak for a user it does not have
		// credentials for.
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Debug("websocket handshake failed", "error", err)
		return
	}
	conn.SetReadLimit(maxMessageSize)

	// A connection that never authenticates is closed rather than left open.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	user, err := h.authenticate(ctx, conn)
	if err != nil {
		h.log.Info("rejected push connection", "error", err, "agent", r.UserAgent())
		conn.Write(ctx, websocket.MessageText, []byte(msgInvalidCreds)) //nolint:errcheck // closing anyway
		conn.Close(websocket.StatusPolicyViolation, "invalid credentials")
		return
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(msgAuthenticated)); err != nil {
		conn.Close(websocket.StatusInternalError, "write failed")
		return
	}

	c := &client{conn: conn, userID: user.ID, send: make(chan notification, sendBuffer)}
	h.add(c)
	defer h.remove(c)
	h.log.Debug("push client connected", "user", user.Username)

	// The reader exists to notice the client going away and to pick up the
	// one command the protocol has; the writer does the real work.
	go h.readCommands(ctx, c, cancel)
	h.writeLoop(ctx, c)

	conn.Close(websocket.StatusNormalClosure, "")
	h.log.Debug("push client disconnected", "user", user.Username)
}

// authenticate performs the protocol's handshake: the client sends a username
// and then a password, each as its own message.
//
// An empty username means the password is a pre-auth token, which is how a
// client that holds a session but not a password connects.
func (h *Hub) authenticate(ctx context.Context, conn *websocket.Conn) (store.User, error) {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	username, err := readText(ctx, conn)
	if err != nil {
		return store.User{}, err
	}
	secret, err := readText(ctx, conn)
	if err != nil {
		return store.User{}, err
	}

	if username == "" {
		userID, ok := h.preAuth.consume(secret)
		if !ok {
			return store.User{}, errors.New("unknown or expired pre-auth token")
		}
		user, err := h.db.UserByID(ctx, userID)
		if err != nil {
			return store.User{}, err
		}
		if user.Disabled {
			return store.User{}, errors.New("account is disabled")
		}
		return user, nil
	}
	return h.auth.Verify(ctx, username, secret)
}

func readText(ctx context.Context, conn *websocket.Conn) (string, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return "", err
	}
	if typ != websocket.MessageText {
		return "", errors.New("expected a text message")
	}
	return string(data), nil
}

// readCommands consumes client messages until the connection closes.
func (h *Hub) readCommands(ctx context.Context, c *client, cancel context.CancelFunc) {
	defer cancel()
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return // the client went away, or ctx ended
		}
		if typ != websocket.MessageText {
			continue
		}
		if string(data) == cmdListenFileID {
			c.wantFileIDs.Store(true)
		}
	}
}

// writeLoop delivers notifications and keeps the connection alive.
func (h *Hub) writeLoop(ctx context.Context, c *client) {
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case n := <-c.send:
			if err := h.deliver(ctx, c, n); err != nil {
				return
			}

		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (h *Hub) deliver(ctx context.Context, c *client, n notification) error {
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Clients that asked for file ids get them, but only when there are ids to
	// send; the protocol falls back to the bare message otherwise.
	if c.wantFileIDs.Load() && len(n.fileIDs) > 0 {
		if err := c.conn.Write(writeCtx, websocket.MessageText, []byte(msgNotifyFileID)); err != nil {
			return err
		}
		payload, err := json.Marshal(n.fileIDs)
		if err != nil {
			return err
		}
		// The ids arrive as their own message, immediately after the marker.
		return c.conn.Write(writeCtx, websocket.MessageText, payload)
	}
	return c.conn.Write(writeCtx, websocket.MessageText, []byte(msgNotifyFile))
}
