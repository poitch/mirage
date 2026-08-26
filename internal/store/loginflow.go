package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LoginFlowTTL bounds how long a pairing session stays usable. It matches the
// window Nextcloud clients assume before they abandon a flow and start over.
const LoginFlowTTL = 20 * time.Minute

// GrantedLogin is the credential handed to a client once a user approves a
// pairing request.
type GrantedLogin struct {
	Username    string
	AppPassword string
}

// ErrLoginFlowPending signals a pairing session that exists but has not yet
// been approved by the user.
var ErrLoginFlowPending = errors.New("login flow pending")

// CreateLoginFlow opens a pairing session.
func (db *DB) CreateLoginFlow(ctx context.Context, pollTokenHash []byte, loginToken, userAgent string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO login_flows (poll_token_hash, login_token, created_at, user_agent)
		VALUES (?, ?, ?, ?)`,
		pollTokenHash, loginToken, time.Now().Unix(), userAgent)
	return err
}

// LoginFlowUserAgent returns the client description recorded for a pending
// pairing session, or ErrNotFound if the token is unknown or expired.
func (db *DB) LoginFlowUserAgent(ctx context.Context, loginToken string) (string, error) {
	var userAgent string
	var createdAt int64
	err := db.QueryRowContext(ctx,
		`SELECT user_agent, created_at FROM login_flows WHERE login_token = ? AND completed_at IS NULL`,
		loginToken).Scan(&userAgent, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if expired(createdAt) {
		return "", ErrNotFound
	}
	return userAgent, nil
}

// GrantLoginFlow approves a pairing session, attaching the app password the
// client will collect on its next poll.
//
// The update is conditional on the session still being pending and unexpired,
// so two concurrent approvals cannot both succeed and mint two tokens.
func (db *DB) GrantLoginFlow(ctx context.Context, loginToken string, userID int64, appPassword string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE login_flows SET user_id = ?, app_password = ?, completed_at = ?
		WHERE login_token = ? AND completed_at IS NULL AND created_at > ?`,
		userID, appPassword, time.Now().Unix(), loginToken, cutoff())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimLoginFlow delivers the granted credential exactly once.
//
// The row is deleted as it is read, in a single statement, so a replayed poll
// cannot retrieve the same app password twice and a leaked poll token is
// useless the moment the real client has collected its credential.
func (db *DB) ClaimLoginFlow(ctx context.Context, pollTokenHash []byte) (GrantedLogin, error) {
	var username, appPassword string
	err := db.QueryRowContext(ctx, `
		DELETE FROM login_flows
		WHERE poll_token_hash = ? AND completed_at IS NOT NULL AND created_at > ?
		RETURNING app_password, (SELECT username FROM users WHERE users.id = login_flows.user_id)`,
		pollTokenHash, cutoff()).Scan(&appPassword, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return GrantedLogin{}, ErrLoginFlowPending
	}
	if err != nil {
		return GrantedLogin{}, err
	}
	return GrantedLogin{Username: username, AppPassword: appPassword}, nil
}

// PruneLoginFlows deletes expired pairing sessions.
func (db *DB) PruneLoginFlows(ctx context.Context) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM login_flows WHERE created_at <= ?`, cutoff())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func cutoff() int64         { return time.Now().Add(-LoginFlowTTL).Unix() }
func expired(at int64) bool { return at <= cutoff() }
