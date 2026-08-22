package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Attachment is one image a work request carries (docs/DATA-API.md): the
// bytes the worker materialises into scratch/attachments/ during workspace
// preparation, plus the name and MIME type needed to write the file. Rows
// live in the store rather than inline in the work request, so a mockup
// stays whole however large it is — the database is the one place it has
// to survive intact.
type Attachment struct {
	ID        string
	Name      string
	MIMEType  string
	Data      []byte
	CreatedAt time.Time
}

// WriteAttachment stores one attachment and returns its id, which the
// producer then carries on the queue request. Only PNG, JPEG, and WebP
// attachments exist (the producers validate that before calling); the store
// itself stores whatever it is given.
func (s *Store) WriteAttachment(ctx context.Context, name, mimeType string, data []byte) (string, error) {
	id := newAttachmentID()
	err := s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attachments (id, name, mime_type, data, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			id, name, mimeType, data, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetAttachment reads one attachment row by id. ErrNotFound when the id is
// unknown — the worker reports that as a setup failure naming the id, since
// a request carrying it cannot be run as intended.
func (s *Store) GetAttachment(ctx context.Context, id string) (Attachment, error) {
	var att Attachment
	var createdAt string
	err := s.readDB.QueryRowContext(ctx, `
		SELECT id, name, mime_type, data, created_at
		FROM attachments WHERE id = ?`, id).
		Scan(&att.ID, &att.Name, &att.MIMEType, &att.Data, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Attachment{}, err
	}
	att.CreatedAt = t
	return att, nil
}

// newAttachmentID mints an attachment id. The "att-" prefix makes its origin
// obvious in a work request's attachment_ids and in a setup failure that
// names a missing row.
func newAttachmentID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand unavailable: " + err.Error())
	}
	return "att-" + hex.EncodeToString(b[:])
}
