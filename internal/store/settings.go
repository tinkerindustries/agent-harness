package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Setting reads one settings row. ok is false when key has no row, so an
// unset key is distinct from a key stored with an empty value.
func (s *Store) Setting(ctx context.Context, key string) (value string, ok bool, err error) {
	row := s.readDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key)
	err = row.Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting upserts one settings row on the single writer goroutine, the
// same path every other write takes. Setting a key that already has a row
// replaces its value and stamps a fresh updated_at.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, updatedAt)
		return err
	})
}

// DeleteSetting removes one settings row. Deleting a key with no row is not
// an error.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, key)
		return err
	})
}

// Settings returns every settings row as a map. The map has no entry for a
// key that has no row.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}
