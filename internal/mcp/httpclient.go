package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// getRaw issues a GET against baseURL+path and returns the response body
// verbatim, for a resource that proxies the harness's read-only API rather
// than reshaping its response.
func getRaw(ctx context.Context, client *http.Client, baseURL, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", path, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

// getJSON issues a GET against baseURL+path and decodes a JSON response
// into out. This is the only way this package reads session or event data
// — it never opens the SQLite database itself (docs/DESIGN.md: "serve is
// the single writer") and instead goes through the same read-only HTTP
// surface docs/DESIGN.md §4.2 defines for the browser.
func getJSON(ctx context.Context, client *http.Client, baseURL, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}

// errNotFound marks a 404 from getJSONOrNotFound. A 404 from GET
// /api/requests/{request_id} is a state, not an error: work_requests rows
// are created at claim time, so a 404 means "the pool has not claimed this
// request yet" (docs/QUEUE-MIGRATION-PLAN.md §5). getJSON collapses every
// non-200 into one status error and keeps doing so for its existing callers;
// this sentinel is what the waiters unwrap instead.
var errNotFound = errors.New("not found")

// getJSONOrNotFound is getJSON with the 404 case reported distinctly: it
// returns errNotFound for a 404 (the resource does not exist) and every
// other non-200 stays the same status error getJSON returns. Callers that
// treat a missing resource as an ordinary state check errors.Is against
// errNotFound rather than parsing the error string.
func getJSONOrNotFound(ctx context.Context, client *http.Client, baseURL, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}

// postJSON issues a POST against baseURL+path with body marshalled as JSON
// and a bearer token on the request. It decodes a JSON response into out.
// This is the package's first non-GET helper: deepseek_stop is the one tool
// that acts on the harness rather than reading it, and it reaches the same
// run-control endpoint the CLI and the browser use (docs/RUN-CONTROL.md
// "MCP and CLI") rather than opening a second path around it. The package
// still opens no SQLite handle — this is an HTTP call like the others.
//
// Any 2xx counts as success (the stop endpoint answers 202). Everything
// else is an error carrying the status and the server's own {"error": "..."}
// message, so a 409 naming the session's actual status surfaces verbatim to
// the caller instead of being replaced by a guess.
func postJSON(ctx context.Context, client *http.Client, baseURL, path, token string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request body for %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}
