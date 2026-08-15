package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// stringList collects a repeated flag into a slice, e.g. -deny "git push"
// -deny "rm -rf".
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runRun drives one or more agent sessions from a single process. A single
// job is the common case: -workspace plus a positional task. Passing
// -workspace and -prompt more than once launches that many jobs
// concurrently against one shared Store, the way docs/DESIGN.md §4.5
// expects the session runner to be exercised — several goroutines, one
// writer, no state shared between them but the store and the client.
func runRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var workspaces, prompts stringList
	fs.Var(&workspaces, "workspace", "workspace directory the agent may read and write; repeat with -prompt to launch several jobs at once")
	fs.Var(&prompts, "prompt", "task for the matching -workspace; if omitted, the task is the trailing positional argument")
	model := fs.String("model", "", "override model (default from config)")
	effort := fs.String("effort", "", "override reasoning effort: low, high, max")
	thinking := fs.Bool("thinking", true, "enable thinking mode")
	maxTokens := fs.Int("max-tokens", 0, "override max_tokens (default from config)")
	permissionMode := fs.String("permission-mode", "", "readonly or full (required)")
	var deny stringList
	fs.Var(&deny, "deny", "deny pattern, matched as a substring; repeatable")
	resultSchemaPath := fs.String("result-schema", "", "path to a JSON Schema file Complete's result must satisfy")
	maxSubTurns := fs.Int("max-sub-turns", 0, "override max sub-turns (default from config)")
	interactive := fs.Bool("interactive", false, "prompt on the terminal for calls the permission policy would otherwise deny (single-job only)")
	debugChurnAt := fs.Int("debug-churn-at-subturn", 0, "debug: deliberately break the shared prefix on this sub-turn to exercise the churn diagnostic (docs/CACHE.md); 0 disables it")
	jobType := fs.String("job-type", agentmeta.JobTypeImplementation, "implementation or orchestration (default implementation)")
	parentAgentType := fs.String("parent-agent-type", "", "the launching agent's kind, as a lowercase slug (claude-code, cursor, ...)")
	parentAgentID := fs.String("parent-agent-id", "", "the launching agent's session id, or the operator's name with -parent-is-user")
	parentIsUser := fs.Bool("parent-is-user", true, "record this run as started by a person, which is the default because harness run is interactive — pass -parent-is-user=false when scripting it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := agentmeta.ValidateJobType(*jobType); err != nil {
		return err
	}
	if err := agentmeta.ValidateParent(*parentIsUser, *parentAgentType, *parentAgentID); err != nil {
		return err
	}

	if len(prompts) == 0 {
		if len(workspaces) != 1 {
			return errors.New("usage: harness run -workspace P \"task\"  (or repeat -workspace and -prompt to launch several jobs)")
		}
		if fs.NArg() < 1 {
			return errors.New("usage: harness run -workspace P \"task\"")
		}
		prompts = stringList{strings.Join(fs.Args(), " ")}
	} else if len(workspaces) != len(prompts) {
		return fmt.Errorf("-workspace given %d time(s) but -prompt given %d time(s); they must match", len(workspaces), len(prompts))
	} else if *interactive && len(prompts) > 1 {
		return errors.New("-interactive is only supported for a single job")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	priceTable, err := pricing.Load(cfg.PriceTablePath)
	if err != nil {
		return err
	}

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	res := settings.NewResolver(st)

	// The run defaults resolve through the settings registry, so a key set
	// with `harness config set` applies here too; the flags below override.
	resolvedModel, err := res.String(ctx, settings.KeyDefaultModel)
	if err != nil {
		return err
	}
	if *model != "" {
		resolvedModel = *model
	}
	resolvedEffort, err := res.String(ctx, settings.KeyDefaultEffort)
	if err != nil {
		return err
	}
	if *effort != "" {
		resolvedEffort = *effort
	}
	resolvedMaxTokens, err := res.Int(ctx, settings.KeyRunMaxTokens)
	if err != nil {
		return err
	}
	if *maxTokens != 0 {
		resolvedMaxTokens = *maxTokens
	}
	resolvedMaxSubTurns, err := resolveMaxSubTurns(ctx, res, resolvedModel)
	if err != nil {
		return err
	}
	if *maxSubTurns != 0 {
		resolvedMaxSubTurns = *maxSubTurns
	}

	if *permissionMode == "" {
		return errors.New("-permission-mode is required: readonly or full")
	}
	mode := tools.Mode(*permissionMode)
	if !mode.Valid() {
		return fmt.Errorf("invalid permission mode %q: must be readonly or full", *permissionMode)
	}

	var resultSchema json.RawMessage
	if *resultSchemaPath != "" {
		b, err := os.ReadFile(*resultSchemaPath)
		if err != nil {
			return fmt.Errorf("read result schema: %w", err)
		}
		resultSchema = b
	}

	rec := newHTTPLogRecorder(cfg)
	defer closeHTTPLog(rec)

	var resolver tools.Resolver
	if *interactive {
		resolver = newInteractiveResolver()
	}

	// The Runner serves whichever provider resolvedModel belongs to, via the
	// same per-model resolver serve uses (clientForModel).
	deepSeekClient := withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(res))
	kimiClient := withKimiHTTPLog(cfg, rec, kimiAPIKeyProvider(res))

	r := &session.Runner{
		Store:       st,
		Mirror:      store.NewMirror(cfg.DataDir),
		Client:      deepSeekClient,
		ClientFor:   func(model string) session.Client { return clientForModel(model, deepSeekClient, kimiClient) },
		Recorder:    rec,
		Prices:      priceTable,
		Gemini:      withGeminiHTTPLog(cfg, rec, googleAPIKeyProvider(res)),
		GeminiModel: googleVisionModelProvider(res),
		Settings:    res,
	}

	fmt.Printf("model %s (effort %s, %s), permission mode %s, %d job(s)\n\n", resolvedModel, resolvedEffort, reasoningClaim(resolvedModel, *thinking), mode, len(workspaces))
	if *debugChurnAt > 0 {
		fmt.Printf("debug: deliberately churning the prefix before sub-turn %d (docs/CACHE.md demonstration)\n\n", *debugChurnAt)
	}

	type jobResult struct {
		label string
		res   *session.RunResult
		err   error
	}
	results := make([]jobResult, len(workspaces))
	var out sync.Mutex // serialises interleaved stdout from concurrent jobs
	var wg sync.WaitGroup
	for i := range workspaces {
		label := fmt.Sprintf("job %d (%s)", i+1, workspaces[i])
		if len(workspaces) == 1 {
			label = "job"
		}
		wg.Add(1)
		go func(i int, label, workspace, prompt string) {
			defer wg.Done()
			res, err := r.Run(ctx, session.RunOptions{
				Model: resolvedModel, Effort: resolvedEffort, Thinking: *thinking, MaxTokens: resolvedMaxTokens,
				Workspace: workspace, PermissionMode: mode, Deny: deny, Prompt: prompt,
				ResultSchema: resultSchema, MaxSubTurns: resolvedMaxSubTurns, Resolver: resolver,
				JobType: *jobType, ParentAgentType: *parentAgentType, ParentAgentID: *parentAgentID,
				ParentIsUser:        *parentIsUser,
				DebugChurnOnSubTurn: *debugChurnAt,
				Progress: func(p session.SubTurnProgress) {
					out.Lock()
					defer out.Unlock()
					fmt.Print(label + ": ")
					printProgress(p)
				},
			})
			results[i] = jobResult{label: label, res: res, err: err}
		}(i, label, workspaces[i], prompts[i])
	}
	wg.Wait()

	var failed bool
	for _, jr := range results {
		fmt.Printf("\n== %s ==\n", jr.label)
		if jr.res != nil {
			printResult(jr.res, priceTable)
			fmt.Printf("mirror: %s\n", filepath.Join(cfg.DataDir, "sessions"))
		}
		if jr.err != nil {
			fmt.Printf("error: %v\n", explainError(jr.err))
			failed = true
		}
	}
	if failed {
		return errors.New("one or more jobs failed")
	}
	return nil
}

// resolveMaxSubTurns resolves the sub-turn budget a run on model gets when
// its caller omits -max-sub-turns: the model's own ceiling when it has one
// (run.max_sub_turns_kimi_k3 for kimi-k3), otherwise the global
// run.max_sub_turns — the same per-model resolution the Runner applies on
// the queue path (internal/session/runner.go,
// docs/KIMI-INTEGRATION.md §3).
func resolveMaxSubTurns(ctx context.Context, res *settings.Resolver, model string) (int, error) {
	if key, _, ok := settings.RunBudgetKeysForModel(model); ok {
		return res.Int(ctx, key)
	}
	return res.Int(ctx, settings.KeyRunMaxSubTurns)
}

func printResult(res *session.RunResult, priceTable *pricing.Table) {
	fmt.Printf("session %s: %s\n", res.SessionID, res.Status)
	if res.Text != "" {
		fmt.Printf("\n%s\n", res.Text)
	}
	if res.Summary != "" {
		fmt.Printf("\nsummary: %s\n", res.Summary)
	}
	if len(res.Result) > 0 {
		fmt.Printf("result: %s\n", res.Result)
	}
	fmt.Printf("\nsub-turns %d, cache hit %d, cache miss %d (%s), completion %d, reasoning %d\n",
		res.SubTurns, res.Usage.CacheHitTokens, res.Usage.CacheMissTokens, cacheHitRate(res.Usage.CacheHitTokens, res.Usage.CacheMissTokens), res.Usage.CompletionTokens, res.Usage.ReasoningTokens)
	fmt.Printf("cost $%.6f USD (price table captured %s)\n", res.Usage.CostUSD, priceTable.CapturedAt)
}

// cacheHitRate formats hit tokens over total prompt tokens as a percentage.
// docs/CACHE.md is explicit that this number alone proves nothing — a
// 200K-token cached prefix with a 500-token miss reports 99.8% whether or
// not anything is wrong — so it is shown alongside the raw hit/miss counts
// and the per-turn churn diagnostic, never in place of either.
func cacheHitRate(hit, miss int) string {
	total := hit + miss
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% hit", float64(hit)/float64(total)*100)
}

// printProgress is the default per-sub-turn line: the churn diagnostic
// docs/CACHE.md names as the useful signal — expected miss against actual
// miss, and whether they disagree enough to call it churn.
func printProgress(p session.SubTurnProgress) {
	u := p.Usage
	churn := ""
	if p.Churned {
		point := "?"
		if u.ChurnPointIndex != nil {
			point = fmt.Sprintf("%d", *u.ChurnPointIndex)
		}
		churn = fmt.Sprintf(" CHURNED (first differing message index %s)", point)
	}
	toolNames := ""
	if len(p.ToolCalls) > 0 {
		toolNames = " tools=" + strings.Join(p.ToolCalls, ",")
	}
	fmt.Printf("[sub-turn %d] prompt=%d hit=%d miss=%d (%s, expected miss %d)%s completion=%d reasoning=%d cost=$%.6f%s\n",
		p.SubTurn, u.PromptTokens, u.PromptCacheHitTokens, u.PromptCacheMissTokens, cacheHitRate(u.PromptCacheHitTokens, u.PromptCacheMissTokens), u.ExpectedMissTokens, churn,
		u.CompletionTokens, u.ReasoningTokens, u.CostUSD, toolNames)
}

// newInteractiveResolver plugs a terminal prompt into the permission
// decision point (docs/DESIGN.md §4.6). Only the CLI registers one;
// queue-driven sessions never do.
func newInteractiveResolver() tools.Resolver {
	reader := bufio.NewReader(os.Stdin)
	return func(toolName, descriptor string) bool {
		fmt.Fprintf(os.Stderr, "\npermission: %s wants to run: %s\nallow this call? [y/N] ", toolName, descriptor)
		line, _ := reader.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "y" || line == "yes"
	}
}
