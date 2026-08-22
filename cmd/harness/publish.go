package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/skills"
)

// runPublish sends one work request to the running harness over POST
// /api/runs — the same endpoint the browser's start form uses — so `harness
// serve` has to be up for it to work. It is an operator and demonstration
// tool, not the harness's only ingress path: any HTTP client can POST the
// same JSON body to /api/runs (docs/DESIGN.md §4.10).
func runPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	var repoFlags stringList
	fs.Var(&repoFlags, "repo", "repository to clone into the run's workspace, as URL[#branch] (branch defaults to main); required, repeatable")
	prompt := fs.String("prompt", "", "task for the request; if omitted, the task is the trailing positional argument")
	requestID := fs.String("request-id", "", "idempotency key; a random one is generated if omitted. Pass the same value twice to demonstrate deduplication")
	model := fs.String("model", "", "override model (config default otherwise)")
	effort := fs.String("effort", "", "override reasoning effort")
	permissionMode := fs.String("permission-mode", "", "readonly or full (required)")
	var deny stringList
	fs.Var(&deny, "deny", "deny pattern, matched as a substring; repeatable")
	resultSchemaPath := fs.String("result-schema", "", "path to a JSON Schema file Complete's result must satisfy")
	maxSubTurns := fs.Int("max-sub-turns", 0, "override max sub-turns")
	deadlineMS := fs.Int64("deadline-ms", 0, "override the request's deadline in milliseconds")
	wait := fs.Bool("wait", false, "block until the final result is published, then print it")
	waitTimeout := fs.Duration("wait-timeout", 0, "how long -wait blocks for (default: the request's own deadline, or the config default deadline)")
	jobType := fs.String("job-type", "", "implementation or orchestration (default implementation)")
	title := fs.String("title", "", "a name for the run, at most 10 words, shown bold on the main page")
	description := fs.String("description", "", "what change this run is making, at most 50 words, shown under the title on the main page")
	phase := fs.Int("phase", 0, "this run's 1-based position in a multi-phase chain; omit (or pair with -total-phases 0) for a standalone run")
	totalPhases := fs.Int("total-phases", 0, "how many phases the chain has in total; omit (or pair with -phase 0) for a standalone run")
	// The three provenance flags are kept for command-line compatibility,
	// but POST /api/runs stamps provenance server-side: handleStartRun
	// overwrites parent_is_user, parent_agent_type and parent_agent_id
	// before validation (docs/RUN-CONTROL.md "Start"), so a value passed
	// here is discarded. The help text says so rather than pretending the
	// flag still has an effect.
	parentAgentType := fs.String("parent-agent-type", "", "the launching agent's kind, as a lowercase slug (claude-code, cursor, ...); has no effect on this path — the API stamps provenance server-side")
	parentAgentID := fs.String("parent-agent-id", "", "the launching agent's session id; has no effect on this path — the API stamps provenance server-side")
	parentIsUser := fs.Bool("parent-is-user", false, "record this run as started by a person rather than an agent; has no effect on this path — the API stamps provenance server-side")
	var skillPacks stringList
	fs.Var(&skillPacks, "skill-pack", "give the run a shipped skill pack; repeatable, off by default. Known packs: "+strings.Join(skills.PackNames(), ", "))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := agentmeta.ValidateJobType(*jobType); err != nil {
		return err
	}
	if len(repoFlags) == 0 {
		return errors.New("usage: harness publish -repo URL[#branch] [flags] \"task\"")
	}
	task := *prompt
	if task == "" {
		if fs.NArg() < 1 {
			return errors.New("usage: harness publish -repo URL[#branch] [flags] \"task\"")
		}
		task = strings.Join(fs.Args(), " ")
	}

	if *permissionMode == "" {
		return errors.New("-permission-mode is required: readonly or full")
	}

	id := *requestID
	if id == "" {
		id = randomRequestID()
	}

	var resultSchema json.RawMessage
	if *resultSchemaPath != "" {
		b, err := os.ReadFile(*resultSchemaPath)
		if err != nil {
			return fmt.Errorf("read result schema: %w", err)
		}
		resultSchema = b
	}

	req := queue.Request{
		RequestID:       id,
		Prompt:          task,
		Repos:           parseRepoFlags(repoFlags),
		Model:           *model,
		Effort:          *effort,
		PermissionMode:  *permissionMode,
		Deny:            deny,
		ResultSchema:    resultSchema,
		MaxSubTurns:     *maxSubTurns,
		DeadlineMS:      *deadlineMS,
		JobType:         *jobType,
		Title:           *title,
		Description:     *description,
		Phase:           *phase,
		TotalPhases:     *totalPhases,
		ParentAgentType: *parentAgentType,
		ParentAgentID:   *parentAgentID,
		ParentIsUser:    *parentIsUser,
		// Nothing unless asked for, the same default the browser start and
		// the MCP launch tool carry (internal/skills, "Packs"). Unlike
		// `harness run`, this path builds the run a workspace of its own, so
		// a pack here lands nowhere anybody has to clean up.
		SkillPacks: skillPacks,
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// The base URL resolves from DEEPSEEK_HARNESS_BASE_URL, falling back to
	// the loopback address DEEPSEEK_HTTP_ADDR names — the same resolution
	// the -wait poll uses, now shared by the publish itself.
	baseURL := os.Getenv("DEEPSEEK_HARNESS_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:" + httpAddrPort(cfg.HTTPAddr)
	}
	client := &http.Client{Timeout: 30 * time.Second}

	// POST /api/runs requires the run-control bearer token, fetched from
	// GET /api/control-token the way internal/mcp/control.go fetches it:
	// the endpoint is loopback-only and unauthenticated
	// (docs/RUN-CONTROL.md "Authentication"). An unreachable harness is
	// named as the cause — the publish posts to the running service, so
	// `harness serve` has to be up.
	token, err := publishControlToken(ctx, client, baseURL)
	if err != nil {
		return fmt.Errorf("could not reach the harness at %s: %v — harness publish posts to the running service; start `harness serve` first", baseURL, err)
	}

	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var out struct {
		RequestID string `json:"request_id"`
	}
	err = publishPost(pubCtx, client, baseURL, token, req, &out)
	cancel()
	if err != nil {
		return fmt.Errorf("publish request: %w", err)
	}
	fmt.Printf("published request_id %s\n", out.RequestID)

	if !*wait {
		return nil
	}

	timeout := *waitTimeout
	if timeout <= 0 {
		// The run deadline is a setting now (run.deadline); publish has no
		// database handle, so it mirrors the setting's default (60 minutes)
		// here. -deadline-ms and -wait-timeout still override.
		timeout = 60 * time.Minute
		if *deadlineMS > 0 {
			timeout = time.Duration(*deadlineMS) * time.Millisecond
		}
	}
	fmt.Printf("waiting up to %s for the final result...\n", timeout)

	// -wait polls the running service's own HTTP API (GET
	// /api/requests/{id}, the same endpoint the browser and the MCP tools
	// read) for the stored result, so `harness serve` has to be up for it
	// to work.
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var row publishRequestRow
		found, err := fetchPublishRequest(ctx, client, baseURL, id, &row)
		if err != nil {
			return fmt.Errorf("could not reach the harness at %s: %v — harness publish -wait polls the running service; start `harness serve` first", baseURL, err)
		}
		// A 404 means the pool has not claimed the request yet; it keeps
		// polling. A claimed row whose finished_at is set holds the result.
		if found && row.FinishedAt != nil {
			var res queue.Result
			if err := json.Unmarshal(row.Result, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return errors.New("no result arrived before the wait timed out")
}

// publishRequestRow is the subset of GET /api/requests/{id}'s wire shape
// -wait needs: the stored result JSON and whether the run has finished. A
// request whose row exists but is not finished has a nil FinishedAt.
type publishRequestRow struct {
	Result     json.RawMessage `json:"result,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// fetchPublishRequest reads one work-request row over HTTP. A 404 — the pool
// has not claimed the request yet, because work_requests rows are created at
// claim time — is reported as found=false with no error: that is the "still
// queued" signal, not a failure (docs/QUEUE-MIGRATION-PLAN.md §5). Every
// other non-200 and every transport error is an error naming the status or
// cause.
func fetchPublishRequest(ctx context.Context, client *http.Client, baseURL, requestID string, out *publishRequestRow) (found bool, err error) {
	url := baseURL + "/api/requests/" + requestID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("decode response from %s: %w", url, err)
	}
	return true, nil
}

// publishControlToken fetches the run-control bearer token POST /api/runs
// requires, from GET /api/control-token — the same loopback-only,
// unauthenticated fetch internal/mcp/control.go makes
// (docs/RUN-CONTROL.md "Authentication"). An empty token is an error: a
// harness that never generated one must fail closed, never send an empty
// bearer that would 503 anyway.
func publishControlToken(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	url := baseURL + "/api/control-token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response from %s: %w", url, err)
	}
	if out.Token == "" {
		return "", errors.New("the harness has no control token configured")
	}
	return out.Token, nil
}

// publishPost POSTs req to /api/runs with the bearer token, the way the
// browser's start form and the MCP launch tool do, and decodes the
// {"request_id": ...} acceptance into out. Any 2xx counts as success (the
// endpoint answers 202); everything else is an error carrying the status
// and the server's own {"error": "..."} message.
func publishPost(ctx context.Context, client *http.Client, baseURL, token string, req queue.Request, out any) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	url := baseURL + "/api/runs"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request for %s: %w", url, err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(r)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// httpAddrPort extracts the port from a host:port HTTP address, for building
// the loopback base URL the publish and its -wait poll fall back to when
// DEEPSEEK_HARNESS_BASE_URL is unset. A malformed address falls back to the
// default port.
func httpAddrPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "8080"
	}
	return port
}

// parseRepoFlags splits each -repo value on the last "#" into a URL and a
// branch. Splitting on the last one keeps a "#" inside a URL intact, and a
// value with none at all clones the default branch.
func parseRepoFlags(values []string) []queue.Repo {
	repos := make([]queue.Repo, 0, len(values))
	for _, v := range values {
		repo := queue.Repo{URL: v}
		if i := strings.LastIndex(v, "#"); i >= 0 {
			repo = queue.Repo{URL: v[:i], Branch: v[i+1:]}
		}
		repos = append(repos, repo)
	}
	return repos
}

func randomRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("publish: crypto/rand unavailable: " + err.Error())
	}
	return "req-" + hex.EncodeToString(b[:])
}
