package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/androiddns"
	"github.com/mrgeoffrich/agent-harness/internal/anthropic"
	"github.com/mrgeoffrich/agent-harness/internal/config"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/stdiosession"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// defaultClaudeModel is what a create body naming no agent.model.id runs on.
const defaultClaudeModel = "claude-sonnet-5-5"

// claudeCustomToolTimeout bounds a pending client-declared tool call under
// claude-session's async flow (docs/STDIO-MANAGED-AGENTS.md, "The seam") —
// deliberately far past any ordinary tool's timeout, since the wait is on a
// client's own answer rather than on work this process is doing itself, and
// that answer's own wall clock is not this harness's to bound. Not truly
// unbounded: a process a stuck call can wedge forever is a worse failure
// mode than a very long, but finite, one. user.interrupt is the documented
// way to escape it sooner (docs/STDIO-MANAGED-AGENTS.md, "user.interrupt").
const claudeCustomToolTimeout = 24 * time.Hour

// runClaudeSession hosts one coding session for a parent process over in and
// out, speaking Anthropic's Managed Agents vocabulary
// (docs/STDIO-MANAGED-AGENTS.md). It hosts the three Claude models alone,
// with the Anthropic key alone — a client speaking this vocabulary has no
// way to drive another vendor's model through it, the same reason
// gemini-session hosts only Google's models.
//
// Nothing may reach stdout but protocol frames, the same rule
// runStdioSession follows and for the same reason.
func runClaudeSession(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("claude-session", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "directory for this session's own SQLite state and transcript mirror (default: a per-process directory under the user cache dir)")
	keepState := fs.Bool("keep-state", false, "leave the state directory behind when the process exits, for reading a finished session's transcript")
	prices := fs.String("prices", "configs/prices.json", "price table, for the cost figures reported on harness.usage")
	model := fs.String("model", "", "model a create body naming no agent.model.id runs on (default "+defaultClaudeModel+")")
	envFile := fs.String("env", "", "read ANTHROPIC_API_KEY from this KEY=VALUE `file` when the environment does not carry it, for driving the process by hand")
	rgBinary := fs.String("rg", "", "`path` to the ripgrep binary the session's Grep calls run (default: $AGENT_HARNESS_RG, then `rg` on the PATH, then Grep's own Go walk)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log.SetOutput(os.Stderr)
	log.SetPrefix("harness claude-session: ")
	if servers := androiddns.Install(); servers != nil {
		log.Printf("no /etc/resolv.conf; resolving DNS through %s", strings.Join(servers, ", "))
	}

	anthropicKey, err := claudeAPIKey(*envFile)
	if err != nil {
		return err
	}

	models := claudeHostedModels()
	chosen := defaultClaudeModel
	if *model != "" {
		if !slices.Contains(models, *model) {
			return fmt.Errorf("-model %s: this process hosts %s", *model, strings.Join(models, ", "))
		}
		chosen = *model
	}

	rgPath, err := tools.RipgrepPath(*rgBinary)
	if err != nil {
		return err
	}

	dir := *stateDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "agent-harness", "claude-session", fmt.Sprint(os.Getpid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	if !*keepState && *stateDir == "" {
		defer os.RemoveAll(dir)
	}

	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	res := settings.NewResolver(st)

	priceTable, err := pricing.Load(*prices)
	if err != nil {
		log.Printf("no price table (%v); harness.usage will report tokens without a cost", err)
		priceTable = nil
	}

	anthropicClient := anthropic.NewClient(anthropic.DefaultBaseURL, anthropic.WithAPIKeyProvider(func() (string, error) {
		return anthropicKey, nil
	}))

	mcpMgr := mcpclient.New(st)
	mcpMgr.EnvFilter = stripProviderAPIKeys
	defer mcpMgr.Close()

	eventHub := hub.New()
	runner := &session.Runner{
		Store:      st,
		Mirror:     store.NewMirror(dir),
		Client:     anthropicClient,
		ClientFor:  func(string) session.Client { return anthropicClient },
		Prices:     priceTable,
		FlashModel: chosen,
		Hub:        eventHub,
		Settings:   res,
		// No vision tool ever reaches this closure: every Claude model
		// reads images natively, so none is offered Glance/Ground/Detect
		// (docs/ANTHROPIC-INTEGRATION.md). Named for the same reason
		// runStdioSession's own unreachable case is: what it would have to
		// be if that ever changed.
		GeminiModel:   func() (string, error) { return defaultGeminiSessionModel, nil },
		ToolEnvFilter: stripProviderAPIKeys,
		ToolTimeouts:  tools.Timeouts{HostTool: claudeCustomToolTimeout},
		RG:            rgPath,
	}

	srv := stdiosession.NewServer(stdiosession.Options{
		Dialect:      stdiosession.NewManagedAgents(),
		Store:        st,
		Runner:       runner,
		Hub:          eventHub,
		MCP:          mcpMgr,
		Models:       models,
		DefaultModel: chosen,
		ServerName:   "agent-harness claude-session",
		HasAPIKey:    func(string) bool { return anthropicKey != "" },
		Version:      buildVersion(),
	})

	log.Printf("ready; default model %s, state under %s", chosen, dir)
	return srv.Serve(ctx, in, out)
}

// claudeHostedModels is every Claude model internal/provider routes — all
// three this binary knows, unconditionally: unlike stdio-session's mixed
// model list, this subcommand hosts one provider's models alone, so there is
// no second provider's key to be missing.
func claudeHostedModels() []string {
	var out []string
	for _, m := range provider.KnownModels() {
		if p, err := provider.ModelFor(m); err == nil && p == provider.Anthropic {
			out = append(out, m)
		}
	}
	return out
}

// claudeAPIKey resolves ANTHROPIC_API_KEY: the environment first, then the
// file -env names — this subcommand's one credential
// (docs/STDIO-MANAGED-AGENTS.md, "Starting the process").
func claudeAPIKey(envFile string) (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if envFile == "" {
		return key, nil
	}
	values, err := config.DotEnvValues(envFile)
	if err != nil {
		return "", fmt.Errorf("-env %s: %w", envFile, err)
	}
	return firstNonEmpty(key, values["ANTHROPIC_API_KEY"]), nil
}
