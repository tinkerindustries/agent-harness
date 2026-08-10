package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
)

// runStop serves `harness stop <session-id> ["reason"]`: it asks the harness
// to end a running session by posting to the same run-control endpoint the
// browser and MCP use (docs/RUN-CONTROL.md "MCP and CLI") — one
// implementation of the verb, not a CLI path that reaches around it.
//
// The control token is read from the http.control_token setting directly,
// the way every other subcommand reads configuration, rather than fetched
// over HTTP (docs/RUN-CONTROL.md "Authentication": the CLI and the harness
// share the settings table). The post is non-blocking on the server's side:
// a 202 is the acceptance, and the run's terminal state arrives over the
// session's own SSE stream — this command reports the acceptance and
// nothing more. Anything but a 202 exits non-zero with the endpoint's own
// message and status.
func runStop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: harness stop <session-id> [\"reason\"]")
	}
	sessionID := fs.Arg(0)
	reason := strings.Join(fs.Args()[1:], " ")

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	res, st, err := openConfigResolver()
	if err != nil {
		return err
	}
	defer st.Close()

	token, err := res.String(ctx, settings.KeyHTTPControlToken)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("run control is not configured: http.control_token is empty (start harness serve once to generate one, or set http.control_token)")
	}

	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	url := "http://" + cfg.HTTPAddr + "/api/sessions/" + sessionID + "/stop"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build stop request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusAccepted {
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("stop %s: status %d: %s", sessionID, resp.StatusCode, msg)
	}

	fmt.Printf("accepted: stopping session %s\n", sessionID)
	if reason != "" {
		fmt.Printf("reason: %s\n", reason)
	}
	fmt.Println("the stop is accepted, not the outcome — the run's terminal state arrives over its SSE stream (status moves to cancelled when the stop lands)")
	return nil
}
