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

// runSteer serves `harness steer <session-id> "text"`: it appends an
// operator instruction to a running session by posting to the same
// run-control endpoint the browser and MCP use (docs/RUN-CONTROL.md "MCP and
// CLI") — one implementation of the verb, not a CLI path that reaches around
// it.
//
// The control token is read from the http.control_token setting directly,
// the way every other subcommand reads configuration, rather than fetched
// over HTTP (docs/RUN-CONTROL.md "Authentication": the CLI and the harness
// share the settings table). The post is non-blocking on the server's side:
// a 202 is the acceptance, carrying the seq the caller's text landed at, and
// the text reaches the model at the next sub-turn boundary — the run does
// not stop to read it, so a long tool call in flight delays it. Anything but
// a 202 exits non-zero with the endpoint's own message and status.
func runSteer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("steer", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: harness steer <session-id> \"text\"")
	}
	sessionID := fs.Arg(0)
	text := strings.Join(fs.Args()[1:], " ")

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

	body, err := json.Marshal(map[string]string{"text": text, "source": "cli"})
	if err != nil {
		return err
	}
	url := "http://" + cfg.HTTPAddr + "/api/sessions/" + sessionID + "/steer"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build steer request: %w", err)
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
		return fmt.Errorf("steer %s: status %d: %s", sessionID, resp.StatusCode, msg)
	}

	var out struct {
		SessionID string `json:"session_id"`
		Seq       int64  `json:"seq"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("steer %s: decode acceptance: %w", sessionID, err)
	}

	fmt.Printf("accepted: steer queued for session %s at seq %d\n", sessionID, out.Seq)
	fmt.Println("the run does not stop to read it — the text reaches the model at the next sub-turn boundary, which may be a minute or more away if a long tool call is in flight")
	return nil
}
