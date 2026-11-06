package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxAttempts is how often a failed extraction is retried before it is
// dropped. With the first failure, a turn fails len(Backoff)+1 times.
const MaxAttempts = 5

// RecordPrompt keeps the latest prompt the user sent to a Claude session.
func (s *Store) RecordPrompt(claudeSession, prompt string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO prompts (claude_session, prompt, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (claude_session) DO UPDATE SET prompt = excluded.prompt, updated_at = excluded.updated_at`,
		claudeSession, prompt, now.Unix())
	return err
}

// LastPrompt returns the latest prompt of a session, or "".
func (s *Store) LastPrompt(claudeSession string) (string, error) {
	var p string
	err := s.db.QueryRow(`SELECT prompt FROM prompts WHERE claude_session = ?`, claudeSession).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return p, err
}

// Known reports whether the store has seen a Claude session: the hooks
// through a prompt or an item, or an earlier backfill, even one that found
// nothing. Backfill skips known sessions. The hooks see every turn, so a
// session they know needs no backfill; one backfilled once is done.
func (s *Store) Known(claudeSession string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT (SELECT count(*) FROM prompts WHERE claude_session = ?) +
		(SELECT count(*) FROM items WHERE claude_session = ?) +
		(SELECT count(*) FROM backfilled WHERE claude_session = ?)`,
		claudeSession, claudeSession, claudeSession).Scan(&n)
	return n > 0, err
}

// InsertBackfill stores the items backfill extracted from a Claude session,
// none or many, and marks the session done, all in one transaction: a
// session is either fully stored and known, or untouched and tried again.
// Items go in as Insert would put them. It returns how many were added.
func (s *Store) InsertBackfill(claudeSession string, items []Item, now time.Time) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, it := range items {
		_, ok, err := insert(tx, it, "?, ?", it.RexSession, it.RexBlock)
		if err != nil {
			return 0, err
		}
		if ok {
			added++
		}
	}
	if _, err := tx.Exec(`INSERT INTO backfilled (claude_session, at) VALUES (?, ?)
		ON CONFLICT (claude_session) DO UPDATE SET at = excluded.at`, claudeSession, now.Unix()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// PendingRow is an extraction waiting to be retried.
type PendingRow struct {
	ID            int64
	ClaudeSession string
	Payload       []byte
}

// Enqueue stores a failed extraction of a Claude session's turn for a later
// retry, due after the first Backoff.
func (s *Store) Enqueue(claudeSession string, payload []byte, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO pending (claude_session, payload, next_at, created_at) VALUES (?, ?, ?, ?)`,
		claudeSession, string(payload), now.Add(Backoff[0]).Unix(), now.Unix())
	return err
}

// Pending returns up to limit queued extractions due at now, oldest first,
// for hooks of other sessions. It offers only each session's oldest row,
// so a later turn never overtakes an earlier one.
func (s *Store) Pending(limit int, now time.Time) ([]PendingRow, error) {
	return s.pending(`SELECT id, claude_session, payload FROM pending AS p
		WHERE next_at <= ? AND id = (SELECT min(id) FROM pending WHERE claude_session = p.claude_session)
		ORDER BY id LIMIT ?`, now.Unix(), limit)
}

// PendingFor returns all of one Claude session's queued extractions, oldest
// first, due or not: the hook holding the session's lock runs them in order.
func (s *Store) PendingFor(claudeSession string) ([]PendingRow, error) {
	return s.pending(`SELECT id, claude_session, payload FROM pending
		WHERE claude_session = ? ORDER BY id`, claudeSession)
}

func (s *Store) pending(q string, args ...any) ([]PendingRow, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingRow
	for rows.Next() {
		var r PendingRow
		var p string
		if err := rows.Scan(&r.ID, &r.ClaudeSession, &p); err != nil {
			return nil, err
		}
		r.Payload = []byte(p)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Backoff is how long a queued extraction waits after each failure: the
// first entry after the turn's own, the next after the first retry, and so
// on, about 2h42m in all.
var Backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// claimSQL counts the attempt and pushes the row back by the wait that
// follows it if it fails, which also covers a retry whose process dies.
// SQLite evaluates SET against the old row, so attempts is the count before
// this one.
var claimSQL = func() string {
	var b strings.Builder
	b.WriteString("UPDATE pending SET attempts = attempts + 1, next_at = ? + CASE attempts")
	for i, d := range Backoff[1:] {
		fmt.Fprintf(&b, " WHEN %d THEN %d", i, int64(d/time.Second))
	}
	fmt.Fprintf(&b, " ELSE %d END WHERE id = ? AND attempts < %d", int64(Backoff[len(Backoff)-1]/time.Second), MaxAttempts)
	return b.String()
}()

// ClaimPending takes a due extraction for one retry, counting the attempt
// and pushing it back by its backoff. It reports false if the row is gone or
// not due, which includes another process having claimed it first. A row
// whose attempts are used up by retries that never reported back is dropped,
// with a *DroppedError.
func (s *Store) ClaimPending(id int64, now time.Time) (bool, error) {
	return s.claim(id, now, " AND next_at <= ?", now.Unix())
}

// ClaimPendingEarly is ClaimPending without waiting for the backoff, for the
// hook that holds the session's lock and runs its turns in order.
func (s *Store) ClaimPendingEarly(id int64, now time.Time) (bool, error) {
	return s.claim(id, now, "")
}

func (s *Store) claim(id int64, now time.Time, cond string, args ...any) (bool, error) {
	if err := s.dropSpent(id); err != nil {
		return false, err
	}
	res, err := s.db.Exec(claimSQL+cond, append([]any{now.Unix(), id}, args...)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DonePending removes a queued extraction that succeeded.
func (s *Store) DonePending(id int64) error {
	_, err := s.db.Exec(`DELETE FROM pending WHERE id = ?`, id)
	return err
}

// FailPending records that a claimed retry failed. ClaimPending already
// counted the attempt; once the attempts are used up, the row is dropped and
// FailPending returns a *DroppedError.
func (s *Store) FailPending(id int64) error {
	return s.dropSpent(id)
}

// DroppedError reports a queued extraction dropped after its last retry. Its
// turn's items are lost, so the hook logs it.
type DroppedError struct {
	ID            int64
	ClaudeSession string
	Attempts      int
}

func (e *DroppedError) Error() string {
	return fmt.Sprintf("dropped queued turn %d of session %s after %d retries", e.ID, e.ClaudeSession, e.Attempts)
}

// dropSpent deletes row id if its attempts are used up, and returns a
// *DroppedError if it did.
func (s *Store) dropSpent(id int64) error {
	d := &DroppedError{ID: id}
	err := s.db.QueryRow(`DELETE FROM pending WHERE id = ? AND attempts >= ? RETURNING claude_session, attempts`,
		id, MaxAttempts).Scan(&d.ClaudeSession, &d.Attempts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return d
}
