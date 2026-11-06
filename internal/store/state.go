package store

import (
	"database/sql"
	"errors"
	"time"
)

// Changes of state only touch items still waiting: an answered or dismissed
// item stays finished.

// Snooze hides an item until a time, or until a trigger fires.
// A snooze with neither would never end, so it is an error.
func (s *Store) Snooze(id int64, until time.Time, trigger string) error {
	if until.IsZero() && trigger == "" {
		return errors.New("snooze needs a time or a trigger")
	}
	var u int64
	if !until.IsZero() {
		u = until.Unix()
	}
	_, err := s.db.Exec(`UPDATE items SET state = 'snoozed', snooze_until = ?, snooze_trigger = ?
		WHERE id = ? AND state IN ('open', 'snoozed')`, u, trigger, id)
	return err
}

// Wake puts a snoozed item back in the open list.
func (s *Store) Wake(id int64) error {
	_, err := s.db.Exec(`UPDATE items SET state = 'open', snooze_until = 0, snooze_trigger = ''
		WHERE id = ? AND state = 'snoozed'`, id)
	return err
}

// Answer marks an item answered with the reply that was sent.
func (s *Store) Answer(id int64, reply string) error {
	_, err := s.db.Exec(`UPDATE items SET state = 'answered', answered_with = ?
		WHERE id = ? AND state IN ('open', 'snoozed')`, reply, id)
	return err
}

// Dismiss marks an item as not a real question.
func (s *Store) Dismiss(id int64) error {
	_, err := s.db.Exec(`UPDATE items SET state = 'dismissed'
		WHERE id = ? AND state IN ('open', 'snoozed')`, id)
	return err
}

// Restore undoes one change to an item: it puts back prev's state, snooze
// and answer, but only while the item is still in the state from, which
// the change left it in. It reports whether it did.
func (s *Store) Restore(prev Item, from string) (bool, error) {
	if prev.State != StateOpen && prev.State != StateSnoozed {
		return false, errors.New("only a waiting item can be restored")
	}
	var u int64
	if !prev.SnoozeUntil.IsZero() {
		u = prev.SnoozeUntil.Unix()
	}
	res, err := s.db.Exec(`UPDATE items SET state = ?, snooze_until = ?, snooze_trigger = ?, answered_with = ?
		WHERE id = ? AND state = ?`, prev.State, u, prev.SnoozeTrigger, prev.AnsweredWith, prev.ID, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// WakeDue reopens every item whose snooze time has passed at now.
func (s *Store) WakeDue(now time.Time) error {
	_, err := s.db.Exec(`UPDATE items SET state = 'open', snooze_until = 0
		WHERE state = 'snoozed' AND snooze_trigger = '' AND snooze_until > 0 AND snooze_until <= ?`, now.Unix())
	return err
}

// WakeTrigger reopens one Claude session's items snoozed on trigger.
func (s *Store) WakeTrigger(claudeSession, trigger string) error {
	_, err := s.db.Exec(`UPDATE items SET state = 'open', snooze_trigger = ''
		WHERE claude_session = ? AND state = 'snoozed' AND snooze_trigger = ?`, claudeSession, trigger)
	return err
}

// execer is a *sql.DB or a *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// releaseBlock forgets a Rex pane for every Claude session except the one
// now running in it, so "jump" and replies never land on the wrong
// conversation.
func releaseBlock(e execer, block, keepClaudeSession string) error {
	_, err := e.Exec(`UPDATE items SET rex_block = '' WHERE rex_block = ? AND claude_session <> ?`, block, keepClaudeSession)
	return err
}

// moveSession points a Claude session's waiting items at the pane it now
// runs in, after a resume.
func moveSession(e execer, claudeSession, rexSession, block string) error {
	_, err := e.Exec(`UPDATE items SET rex_session = ?, rex_block = ?
		WHERE claude_session = ? AND state IN ('open', 'snoozed')`, rexSession, block, claudeSession)
	return err
}

// ClaimPane records that claudeSession now runs in a Rex pane. It is the
// only writer of pane ownership: SessionStart calls it on startup, resume and
// /clear, so it sees every session that runs in a pane. In one transaction
// it makes the session the pane's owner, forgets the pane for every other
// session, and points the session's waiting items at it.
func (s *Store) ClaimPane(block, rexSession, claudeSession string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO panes (block, claude_session, rex_session, claimed_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (block) DO UPDATE SET claude_session = excluded.claude_session,
		rex_session = excluded.rex_session, claimed_at = excluded.claimed_at`,
		block, claudeSession, rexSession, now.Unix()); err != nil {
		return err
	}
	// A session runs in one pane at a time: after a resume elsewhere, the
	// old pane is no longer its.
	if _, err := tx.Exec(`DELETE FROM panes WHERE claude_session = ? AND block <> ?`, claudeSession, block); err != nil {
		return err
	}
	if err := releaseBlock(tx, block, claudeSession); err != nil {
		return err
	}
	if err := moveSession(tx, claudeSession, rexSession, block); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimUnownedPane lets a session that never ran SessionStart (it started
// before the hooks were installed) take the Rex pane its Stop hook runs in.
// It claims block only if no session owns it and claudeSession owns no
// pane, both checked in the INSERT itself, and then does what ClaimPane
// does for the session's items. A SessionStart claim always wins: this
// never overwrites an owner, and a later ClaimPane of another conversation
// in the pane releases these items as usual. It reports whether it claimed.
func (s *Store) ClaimUnownedPane(block, rexSession, claudeSession string, now time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO panes (block, claude_session, rex_session, claimed_at)
		SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM panes WHERE block = ? OR claude_session = ?)`,
		block, claudeSession, rexSession, now.Unix(), block, claudeSession)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if err := releaseBlock(tx, block, claudeSession); err != nil {
		return false, err
	}
	if err := moveSession(tx, claudeSession, rexSession, block); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// PaneOwner returns the Claude session that last claimed block, or "" when
// none owns it.
func (s *Store) PaneOwner(block string) (string, error) {
	var owner string
	err := s.db.QueryRow(`SELECT claude_session FROM panes WHERE block = ?`, block).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return owner, err
}
