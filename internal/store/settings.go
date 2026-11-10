package store

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Setting returns the value of a rexq setting, or "" when it is unset.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting sets a rexq setting; an empty value unsets it.
func (s *Store) SetSetting(key, value string) error {
	if value == "" {
		_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// A choice rexq asks the user for once per store, such as the Pi
// extraction model, has an ask state, kept as the setting key+"-asked":
// AskNever until someone is asked, AskOpen while a picker shows it, and
// AskDone once it is answered, by a choice (Automatic included), by
// dismissing the picker, or by setting the value from outside.
const (
	AskNever = "never"
	AskOpen  = "open"
	AskDone  = "done"
)

// AskWait is how long an open picker holds things back. One that never
// answers (Pi crashed, or was quit with it open) counts as answered after
// it: asked once, and nothing waits forever.
const AskWait = 10 * time.Minute

const askedOpen = "open:"

// AskState returns key's ask state at now.
func (s *Store) AskState(key string, now time.Time) (string, error) {
	v, err := s.Setting(key + "-asked")
	switch {
	case err != nil:
		return "", err
	case v == "":
		return AskNever, nil
	case strings.HasPrefix(v, askedOpen):
		at, err := strconv.ParseInt(strings.TrimPrefix(v, askedOpen), 10, 64)
		if err == nil && now.Before(time.Unix(at, 0).Add(AskWait)) {
			return AskOpen, nil
		}
	}
	return AskDone, nil
}

// ClaimAsk marks key as asked, at now, if nobody was asked yet. It reports
// whether this call did, and so is the one to show the picker: two
// sessions starting together don't both ask.
func (s *Store) ClaimAsk(key string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`,
		key+"-asked", askedOpen+strconv.FormatInt(now.Unix(), 10))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DismissAsk answers key without a choice: the picker was dismissed, and
// the value stays as it is.
func (s *Store) DismissAsk(key string) error {
	return s.SetSetting(key+"-asked", AskDone)
}

// SetChoice sets key to value ("" for automatic) and answers it, in one
// transaction.
func (s *Store) SetChoice(key, value string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if value == "" {
		_, err = tx.Exec(`DELETE FROM settings WHERE key = ?`, key)
	} else {
		_, err = tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key+"-asked", AskDone); err != nil {
		return err
	}
	return tx.Commit()
}
