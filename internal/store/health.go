package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Health is how extraction has been going, so the panel can say when items
// stop arriving (a logged-out claude, a usage limit) instead of looking
// like there is nothing to answer.
type Health struct {
	LastError   string
	LastErrorAt time.Time // zero if extraction never failed
	LastOKAt    time.Time // zero if it never succeeded
	Pending     int       // turns queued for a retry
}

// Failing reports whether the last extraction failed: an error came after
// the last success.
func (h Health) Failing() bool {
	return !h.LastErrorAt.IsZero() && h.LastErrorAt.After(h.LastOKAt)
}

// maxHealthError is how much of an error Health keeps: enough to say what
// went wrong. hook.log has the rest.
const maxHealthError = 300

// RecordExtractionError notes that an extraction failed with msg at now.
func (s *Store) RecordExtractionError(msg string, now time.Time) error {
	if len(msg) > maxHealthError {
		msg = strings.ToValidUTF8(msg[:maxHealthError], "") + "…"
	}
	_, err := s.db.Exec(`INSERT INTO health (id, last_error, last_error_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET last_error = excluded.last_error, last_error_at = excluded.last_error_at`,
		Sanitize(msg), now.Unix())
	return err
}

// RecordExtractionOK notes that an extraction succeeded at now.
func (s *Store) RecordExtractionOK(now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO health (id, last_ok_at) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET last_ok_at = excluded.last_ok_at`, now.Unix())
	return err
}

// Health returns how extraction has been going.
func (s *Store) Health() (Health, error) {
	var h Health
	var errAt, okAt int64
	err := s.db.QueryRow(`SELECT last_error, last_error_at, last_ok_at FROM health WHERE id = 1`).
		Scan(&h.LastError, &errAt, &okAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return h, err
	}
	if errAt > 0 {
		h.LastErrorAt = time.Unix(errAt, 0)
	}
	if okAt > 0 {
		h.LastOKAt = time.Unix(okAt, 0)
	}
	err = s.db.QueryRow(`SELECT count(*) FROM pending`).Scan(&h.Pending)
	return h, err
}
