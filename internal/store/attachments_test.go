package store

import (
	"context"
	"errors"
	"testing"
)

// TestAttachmentRoundTrip writes an attachment, reads it back by the id the
// write returned, and checks every field survived: the producer stores the
// bytes once and the worker reads exactly them, so a mockup reaches the
// workspace byte for byte.
func TestAttachmentRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, err := s.WriteAttachment(ctx, "mockup.png", "image/png", []byte("the image bytes"))
	if err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	if id == "" || id[:4] != "att-" {
		t.Fatalf("attachment id = %q, want an att- prefixed id", id)
	}

	got, err := s.GetAttachment(ctx, id)
	if err != nil {
		t.Fatalf("get attachment: %v", err)
	}
	if got.ID != id || got.Name != "mockup.png" || got.MIMEType != "image/png" {
		t.Errorf("attachment = %+v, want id %s, name mockup.png, mime image/png", got, id)
	}
	if string(got.Data) != "the image bytes" {
		t.Errorf("data = %q, want the bytes written", got.Data)
	}
	if got.CreatedAt.IsZero() {
		t.Error("expected a non-zero created_at")
	}

	// A second write is a second row: two attachments in one request keep
	// their own ids and bytes.
	id2, err := s.WriteAttachment(ctx, "light.png", "image/png", []byte("second"))
	if err != nil {
		t.Fatalf("write second attachment: %v", err)
	}
	if id2 == id {
		t.Fatal("two writes must not share an id")
	}
	got2, err := s.GetAttachment(ctx, id2)
	if err != nil {
		t.Fatalf("get second attachment: %v", err)
	}
	if string(got2.Data) != "second" || got2.Name != "light.png" {
		t.Errorf("second attachment = %+v, want its own bytes and name", got2)
	}
}

// TestGetAttachmentUnknownID pins that a missing row is ErrNotFound, the
// shape the worker's setup failure path reads.
func TestGetAttachmentUnknownID(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetAttachment(context.Background(), "att-0000000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
