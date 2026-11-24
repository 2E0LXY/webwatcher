package app

import (
	"database/sql"
	"errors"
	"strings"
)

// Fetch modes of a watch. A server watch is fetched by this server's scheduler;
// an agent watch is only ever fetched by the owner's Home Agent, and this server
// never requests the page itself.
const (
	FetchServer = "server"
	FetchAgent  = "agent"
)

// AgentTokenPrefix marks agent tokens so they are recognisable in a secret scanner.
const AgentTokenPrefix = "wwa_"

// MaxAgentTokensPerUser bounds the active agent tokens of one account.
const MaxAgentTokensPerUser = 3

var ErrAgentTokenCap = errors.New("agent token limit reached")

// newAgentToken returns a 256-bit random token and its SHA-256 hex hash. Only
// the hash is stored; the token is shown once.
func newAgentToken() (raw, hash string) {
	t, _ := newToken()
	raw = AgentTokenPrefix + t
	return raw, hashToken(raw)
}

// AgentTokenInfo describes a token without ever carrying the secret.
type AgentTokenInfo struct {
	ID         int64
	Label      string
	CreatedAt  int64
	LastSeenAt *int64
	RevokedAt  *int64
}

// CreateAgentToken stores the hash of a new token, enforcing the cap on active tokens atomically.
func (s *Store) CreateAgentToken(uid int64, label, hash string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_tokens WHERE user_id=? AND revoked_at IS NULL`, uid).Scan(&n); err != nil {
		return 0, err
	}
	if n >= MaxAgentTokensPerUser {
		return 0, ErrAgentTokenCap
	}
	res, err := tx.Exec(`INSERT INTO agent_tokens(user_id, token_hash, label, created_at) VALUES(?,?,?,?)`, uid, hash, label, s.now())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

// RotateAgentToken replaces the hash of an active token the user owns, so the
// old secret stops working at once. The label and id are kept.
func (s *Store) RotateAgentToken(uid, id int64, hash string) error {
	res, err := s.db.Exec(`UPDATE agent_tokens SET token_hash=?, created_at=?, last_seen_at=NULL WHERE id=? AND user_id=? AND revoked_at IS NULL`,
		hash, s.now(), id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeAgentToken revokes an active token the user owns.
func (s *Store) RevokeAgentToken(uid, id int64) error {
	res, err := s.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`, s.now(), id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAgentTokens returns the user's tokens, active ones first. Revoked tokens
// are kept for the audit trail but only the active ones count towards the cap.
func (s *Store) ListAgentTokens(uid int64) ([]AgentTokenInfo, error) {
	rows, err := s.db.Query(`SELECT id, label, created_at, last_seen_at, revoked_at FROM agent_tokens WHERE user_id=?
		ORDER BY revoked_at IS NOT NULL, id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentTokenInfo
	for rows.Next() {
		var t AgentTokenInfo
		if err := rows.Scan(&t.ID, &t.Label, &t.CreatedAt, &t.LastSeenAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AgentByHash resolves a token hash to its owner, only while the token is not
// revoked and the account is not disabled. It records last_seen_at. The stored
// hash is compared in constant time as well as by index.
func (s *Store) AgentByHash(hash string) (tokenID, uid int64, err error) {
	var stored string
	err = s.db.QueryRow(`SELECT t.id, t.user_id, t.token_hash FROM agent_tokens t JOIN users u ON u.id=t.user_id
		WHERE t.token_hash=? AND t.revoked_at IS NULL AND u.disabled_at IS NULL`, hash).Scan(&tokenID, &uid, &stored)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !ctEqual(stored, hash)) {
		return 0, 0, ErrInvalidToken
	}
	if err != nil {
		return 0, 0, err
	}
	_, err = s.db.Exec(`UPDATE agent_tokens SET last_seen_at=? WHERE id=?`, s.now(), tokenID)
	return tokenID, uid, err
}

// AgentLastSeen is the newest last_seen_at over the user's active tokens, or nil.
func (s *Store) AgentLastSeen(uid int64) (*int64, error) {
	var t *int64
	err := s.db.QueryRow(`SELECT MAX(last_seen_at) FROM agent_tokens WHERE user_id=? AND revoked_at IS NULL`, uid).Scan(&t)
	return t, err
}

func validFetchMode(m string) bool { return m == FetchServer || m == FetchAgent }

// SetFetchMode changes where a watch is fetched from. The version is bumped so
// a result that was already in flight under the old mode is dropped, and the
// watch is made due at once with its failure run cleared.
func (s *Store) SetFetchMode(uid, id int64, mode string) error {
	if !validFetchMode(mode) {
		return errors.New("invalid fetch mode")
	}
	res, err := s.db.Exec(`UPDATE watches SET fetch_mode=?, version=version+1, failures=0, failure_notified=0,
		status=CASE WHEN status IN ('blocked','error','noprice','disallowed') THEN 'pending' ELSE status END, next_check_at=?
		WHERE id=? AND user_id=? AND fetch_mode<>?`, mode, s.now(), id, uid, mode)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either not the user's watch or already in that mode.
		var cur string
		if err := s.db.QueryRow(`SELECT fetch_mode FROM watches WHERE id=? AND user_id=?`, id, uid).Scan(&cur); err != nil {
			return ErrNotFound
		}
	}
	return nil
}

// FetchModes returns the fetch mode of each of the user's watches, by id.
func (s *Store) FetchModes(uid int64) (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT id, fetch_mode FROM watches WHERE user_id=?`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var m string
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// AgentDueWatches lists the user's agent watches that are due, oldest first.
func (s *Store) AgentDueWatches(uid int64, limit int) ([]*Watch, error) {
	rows, err := s.db.Query(`SELECT `+s.watchCols()+` FROM watches w JOIN users u ON u.id=w.user_id
		WHERE u.disabled_at IS NULL AND w.user_id=? AND w.fetch_mode='agent' AND w.paused=0 AND w.failures<? AND w.next_check_at<=?
		ORDER BY w.next_check_at LIMIT ?`, uid, MaxFailures, s.now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// AgentOffline reports whether an agent watch has had no result for three
// intervals. A watch that has never been checked counts from its creation. A
// deliberate back-off (the next check is further away than that) is not offline.
func AgentOffline(w *Watch, now int64) bool {
	iv := int64(w.IntervalMin)
	if iv <= 0 {
		iv = DefaultIntervalMin
	}
	last := w.CreatedAt
	if w.LastCheckedAt != nil {
		last = *w.LastCheckedAt
	}
	deadline := max(last+3*iv*60, w.NextCheckAt+iv*60)
	return now > deadline
}

// agentHost is the lower-case hostname of a watch URL.
func agentHost(raw string) string { return strings.ToLower(hostOf(raw)) }
