// Package store is the harness's SQLite persistence: sessions, their event
// logs, work requests, workspace leases, and settings (docs/DESIGN.md §4.8).
//
// All writes funnel through one goroutine fed by a channel, so SQLITE_BUSY
// never arises from our own concurrency (docs/DESIGN.md §4.5). Reads use a
// separate connection pool and never touch the write path. WAL mode lets
// those reads proceed concurrently with the writer.
//
// Split by concern: this file holds the Store type and the single-writer
// loop; errors.go the error vocabulary; schema.go the SQL schema and its
// column migration; sessions.go the session status vocabulary and CRUD;
// events.go the event payload types and the append-only log's queries;
// leases.go workspace leases; work_requests.go work requests; settings.go
// the settings table; evals.go eval runs and members; mirror.go and diff.go
// the disk mirror; summary.go the session-list usage summaries.
package store

import (
	"context"
	"database/sql"

	_ "modernc.org/sqlite"
)

// Store owns the SQLite handle pair and the writer goroutine.
type Store struct {
	writeDB *sql.DB
	readDB  *sql.DB
	jobs    chan job
	stopped chan struct{}
}

type job struct {
	fn   func(*sql.Tx) error
	resp chan error
}

// Close stops the writer goroutine and closes both connection pools. It
// waits for any in-flight write to finish first.
func (s *Store) Close() error {
	close(s.jobs)
	<-s.stopped
	if err := s.writeDB.Close(); err != nil {
		return err
	}
	return s.readDB.Close()
}

func (s *Store) writerLoop() {
	defer close(s.stopped)
	for j := range s.jobs {
		j.resp <- s.runJob(j)
	}
}

func (s *Store) runJob(j job) error {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	if err := j.fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// submit runs fn inside a transaction on the single writer goroutine and
// waits for the result.
func (s *Store) submit(ctx context.Context, fn func(*sql.Tx) error) error {
	resp := make(chan error, 1)
	select {
	case s.jobs <- job{fn: fn, resp: resp}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
