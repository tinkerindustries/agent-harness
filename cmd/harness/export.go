package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// runExport rebuilds a session's disk mirror from the database. It doubles
// as the repair path after a crash between the DB commit and the disk
// write (docs/DESIGN.md §4.8).
func runExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: harness export <session-id>")
	}
	sessionID := fs.Arg(0)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	mirror := store.NewMirror(cfg.DataDir)
	if err := store.ExportTo(ctx, st, mirror, sessionID); err != nil {
		return err
	}

	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	fmt.Println(mirror.Dir(sess))
	return nil
}
