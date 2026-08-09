package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	harnessmcp "github.com/mrgeoffrich/deepseek-harness/internal/mcp"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// runMCP starts the MCP launch server: a streamable-HTTP endpoint that lets
// an external agent harness start and collect deepseek-harness runs
// (docs/DESIGN.md's MCP launch server, §4.10). It is a separate process from
// `harness serve`: it holds a NATS connection and nothing else, never the
// SQLite handle serve owns, and depends on serve having already declared the
// WORK and RESULTS streams — it does not declare them itself, so it can
// never disagree with the running pool about MaxAckPending.
func runMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	addr := fs.String("addr", "", "override the MCP HTTP address (default from config; loopback)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadMCP()
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	nc, js, err := queue.Connect(cfg.NATSURL)
	if err != nil {
		return err
	}
	defer nc.Close()

	warnIfStreamsMissing(ctx, js)

	svc := &harnessmcp.Service{
		JS:         js,
		Cfg:        cfg,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Registry:   harnessmcp.NewRegistry(),
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", svc.Handler())
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("harness mcp: http shutdown: %v", err)
		}
	}()

	log.Printf("harness mcp: connected to %s, permission ceiling %s",
		cfg.NATSURL, cfg.PermissionCeiling)
	log.Printf("harness mcp: listening on %s/mcp", cfg.Addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// warnIfStreamsMissing only logs: harness mcp does not own the WORK and
// RESULTS streams (harness serve declares them, docs/DESIGN.md §4.10), so it
// cannot fix a missing one, only tell an operator every launch will fail to
// publish until serve has started at least once.
func warnIfStreamsMissing(ctx context.Context, js jetstream.JetStream) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, name := range []string{queue.StreamWork, queue.StreamResults} {
		if _, err := js.Stream(checkCtx, name); err != nil {
			log.Printf("harness mcp: warning: stream %s not found (%v); launches will fail to publish until harness serve has started at least once", name, err)
		}
	}
}
