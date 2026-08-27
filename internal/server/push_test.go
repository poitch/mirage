package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialPush opens the push websocket and completes the protocol handshake:
// username, then password, then the server's acknowledgement.
func (h *harness) dialPush(t *testing.T, username, password string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(h.http.URL, "http") + "/push/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial push: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	writeText(t, conn, username)
	writeText(t, conn, password)
	return conn
}

func writeText(t *testing.T, conn *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}

// expectMessage reads one message, failing if none arrives in time.
func expectMessage(t *testing.T, conn *websocket.Conn, what string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
	return string(data)
}

func TestPushAuthentication(t *testing.T) {
	h := newHarness(t)

	conn := h.dialPush(t, "alice", alicePassword)
	if got := expectMessage(t, conn, "the authentication reply"); got != "authenticated" {
		t.Fatalf("reply = %q, want %q", got, "authenticated")
	}
}

// TestPushRejectsBadCredentials: the websocket is not behind the HTTP auth
// middleware, so it is solely responsible for who gets in.
func TestPushRejectsBadCredentials(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct{ name, user, pass string }{
		{"wrong password", "alice", "not-the-password"},
		{"unknown user", "nobody", "whatever"},
		{"empty password", "alice", ""},
		{"bogus pre-auth token", "", "not-a-real-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := h.dialPush(t, tc.user, tc.pass)
			got := expectMessage(t, conn, "the rejection")
			if got == "authenticated" {
				t.Fatal("bad credentials were accepted")
			}
			if !strings.HasPrefix(got, "err:") {
				t.Errorf("reply = %q, want an error", got)
			}
		})
	}
}

// TestPushNotifiesOnClientWrite is the point of M6: a change made by one client
// reaches another in about a second rather than on the poll interval.
func TestPushNotifiesOnClientWrite(t *testing.T) {
	h := newHarness(t)
	conn := h.dialPush(t, "alice", alicePassword)
	if got := expectMessage(t, conn, "authentication"); got != "authenticated" {
		t.Fatalf("reply = %q", got)
	}

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/pushed.txt",
		"alice", alicePassword, "written by another client", nil)
	resp.Body.Close()

	if got := expectMessage(t, conn, "a change notification"); got != "notify_file" {
		t.Errorf("notification = %q, want notify_file", got)
	}
}

// TestPushSendsFileIDsWhenAsked covers the opt-in extension: a client that
// asked for ids gets them as a separate message after the marker.
func TestPushSendsFileIDsWhenAsked(t *testing.T) {
	h := newHarness(t)
	conn := h.dialPush(t, "alice", alicePassword)
	expectMessage(t, conn, "authentication")

	writeText(t, conn, "listen notify_file_id")
	// The command is handled by the reader goroutine, so give it a moment to
	// land before provoking a change.
	time.Sleep(100 * time.Millisecond)

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/ided.txt",
		"alice", alicePassword, "content", nil)
	resp.Body.Close()

	if got := expectMessage(t, conn, "the file-id marker"); got != "notify_file_id" {
		t.Fatalf("marker = %q, want notify_file_id", got)
	}
	payload := expectMessage(t, conn, "the file id list")
	if !strings.HasPrefix(payload, "[") || !strings.HasSuffix(payload, "]") {
		t.Errorf("id list = %q, want a JSON array", payload)
	}
}

// TestPushIsolatesUsers: a notification must never tell one account that
// another's files changed.
func TestPushIsolatesUsers(t *testing.T) {
	h := newHarness(t)
	conn := h.dialPush(t, "bob", bobPassword)
	if got := expectMessage(t, conn, "authentication"); got != "authenticated" {
		t.Fatalf("reply = %q", got)
	}

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/private.txt",
		"alice", alicePassword, "alice's change", nil)
	resp.Body.Close()

	// Bob must hear nothing at all from a change in alice's account.
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, data, err := conn.Read(ctx); err == nil {
		t.Fatalf("bob was notified about alice's change: %q", data)
	}
}

// TestPushNotifiesOnOutOfBandChange joins M5 to M6: a file arriving over SMB
// should wake clients just as one uploaded through Mirage does.
func TestPushNotifiesOnOutOfBandChange(t *testing.T) {
	h := newHarness(t)
	conn := h.dialPush(t, "alice", alicePassword)
	expectMessage(t, conn, "authentication")

	writeFile(t, filepath.Join(h.homes["alice"], "from-smb.txt"), "arrived out of band")
	if err := h.server.scanner.ScanAll(context.Background(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	if got := expectMessage(t, conn, "a change notification"); got != "notify_file" {
		t.Errorf("notification = %q, want notify_file", got)
	}
}

// TestScanWithoutChangesIsSilent: a scan runs on a timer, so notifying
// unconditionally would wake every client every interval for nothing.
func TestScanWithoutChangesIsSilent(t *testing.T) {
	h := newHarness(t)
	conn := h.dialPush(t, "alice", alicePassword)
	expectMessage(t, conn, "authentication")

	if err := h.server.scanner.ScanAll(context.Background(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, data, err := conn.Read(ctx); err == nil {
		t.Fatalf("an unchanged rescan notified clients: %q", data)
	}
}

func TestPushPreAuthToken(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodGet, "/index.php/apps/notify_push/pre_auth", "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre_auth: status = %d, want 200", resp.StatusCode)
	}
	token := strings.TrimSpace(readBody(t, resp))
	if token == "" {
		t.Fatal("pre_auth returned an empty token")
	}

	// An empty username means the second message is a pre-auth token.
	conn := h.dialPush(t, "", token)
	if got := expectMessage(t, conn, "authentication"); got != "authenticated" {
		t.Fatalf("reply = %q, want authenticated", got)
	}

	// The token is single-use, so a leaked one is worth nothing once redeemed.
	conn2 := h.dialPush(t, "", token)
	if got := expectMessage(t, conn2, "the rejection"); got == "authenticated" {
		t.Error("a pre-auth token was accepted twice")
	}
}

func TestPushPreAuthRequiresAuthentication(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/index.php/apps/notify_push/pre_auth", "", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPushIsAdvertised(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", "", "", "", nil)
	body := readBody(t, resp)

	if !strings.Contains(body, `"notify_push"`) {
		t.Fatalf("capabilities do not advertise notify_push; got:\n%s", body)
	}
	// The harness is configured with an https external URL, so the websocket
	// endpoint must use wss - a mismatched scheme is refused on upgrade.
	if !strings.Contains(body, `"websocket":"wss://mirage.test/push/ws"`) {
		t.Errorf("websocket endpoint is wrong or not wss; got:\n%s", body)
	}
	if !strings.Contains(body, `"pre_auth":"https://mirage.test/index.php/apps/notify_push/pre_auth"`) {
		t.Errorf("pre_auth endpoint is wrong; got:\n%s", body)
	}
}

// TestPushSurvivesDisconnect: clients drop and reconnect constantly, and a
// departed connection must not keep the hub from serving the rest.
func TestPushSurvivesDisconnect(t *testing.T) {
	h := newHarness(t)

	first := h.dialPush(t, "alice", alicePassword)
	expectMessage(t, first, "authentication")
	first.CloseNow()

	second := h.dialPush(t, "alice", alicePassword)
	expectMessage(t, second, "authentication")

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/after-reconnect.txt",
		"alice", alicePassword, "content", nil)
	resp.Body.Close()

	if got := expectMessage(t, second, "a change notification"); got != "notify_file" {
		t.Errorf("notification = %q, want notify_file", got)
	}
	_ = os.Remove(filepath.Join(h.homes["alice"], "after-reconnect.txt"))
}
