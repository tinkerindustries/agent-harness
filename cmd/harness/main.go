// Command harness is a CLI for talking to DeepSeek's native API directly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
)

const usage = `usage: harness <command> [flags]

commands:
  ask "..."                    send a prompt and stream reasoning and content to the terminal
  run -workspace P "..."       run the agent loop against a workspace until it finishes or gives up
  serve                        pull work requests from NATS JetStream and run them as a worker pool
  mcp                          run an MCP server that launches and collects harness runs over NATS
  publish -repo URL "..."      publish a work request to the queue "harness serve" reads
  resume <session-id> ["..."]  continue a finished, failed, or timed-out session
  delete <session-id>          remove a session and its event log (refuses a running one)
  export <session-id>          rebuild a session's disk mirror from the database
  models                       list available models
  balance                      show account balance

run and publish both require -permission-mode, readonly or full. publish's
-repo takes URL[#branch] and repeats; run's -workspace repeats too, paired
with a -prompt each. "harness <command> -h" lists that command's flags.`

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "harness: warning: reading .env: %v\n", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "ask":
		err = runAsk(ctx, os.Args[2:])
	case "run":
		err = runRun(ctx, os.Args[2:])
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "mcp":
		err = runMCP(ctx, os.Args[2:])
	case "publish":
		err = runPublish(ctx, os.Args[2:])
	case "resume":
		err = runResume(ctx, os.Args[2:])
	case "delete":
		err = runDelete(ctx, os.Args[2:])
	case "export":
		err = runExport(ctx, os.Args[2:])
	case "models":
		err = runModels(ctx, os.Args[2:])
	case "balance":
		err = runBalance(ctx, os.Args[2:])
	case "-h", "-help", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness: "+err.Error())
		os.Exit(1)
	}
}

func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("%w (see .env.example)", err)
	}
	return cfg, nil
}

func explainError(err error) error {
	if deepseek.IsInsufficientBalance(err) {
		return fmt.Errorf("account balance is exhausted (HTTP 402): %w", err)
	}
	return err
}

func runAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	model := fs.String("model", "", "override model (default from config)")
	effort := fs.String("effort", "", "override reasoning effort: low, high, max")
	thinking := fs.Bool("thinking", true, "enable thinking mode")
	maxTokens := fs.Int("max-tokens", 0, "override max_tokens (default from config)")
	system := fs.String("system", "", "optional system message")
	jobType := fs.String("job-type", agentmeta.JobTypeImplementation, "implementation or orchestration (default implementation)")
	parentAgentType := fs.String("parent-agent-type", agentmeta.ParentAgentUser, "the agent that owns this session, or \"user\"")
	parentAgentID := fs.String("parent-agent-id", "", "that agent's session id; must be empty when the type is user")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := agentmeta.ValidateJobType(*jobType); err != nil {
		return err
	}
	if err := agentmeta.ValidateParentAgent(*parentAgentType, *parentAgentID); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: harness ask [flags] \"prompt\"")
	}
	prompt := strings.Join(fs.Args(), " ")

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *model != "" {
		cfg.Model = *model
	}
	if *effort != "" {
		cfg.Effort = *effort
	}
	if *maxTokens != 0 {
		cfg.MaxTokens = *maxTokens
	}
	cfg.Thinking = *thinking

	priceTable, err := pricing.Load(cfg.PriceTablePath)
	if err != nil {
		return err
	}

	client := deepseek.NewClient(cfg.BaseURL, cfg.APIKey)

	var messages []deepseek.Message
	if *system != "" {
		messages = append(messages, deepseek.SystemMessage(*system))
	}
	messages = append(messages, deepseek.UserMessage(prompt))

	thinkingType := deepseek.ThinkingDisabled
	if cfg.Thinking {
		thinkingType = deepseek.ThinkingEnabled
	}

	req := deepseek.ChatCompletionRequest{
		Model:           cfg.Model,
		Messages:        messages,
		Thinking:        &deepseek.ThinkingConfig{Type: thinkingType},
		ReasoningEffort: cfg.Effort,
		MaxTokens:       cfg.MaxTokens,
	}

	start := time.Now()
	events, err := client.StreamChatCompletion(ctx, req)
	if err != nil {
		return explainError(err)
	}

	var reasoningOpen, contentOpen bool
	var contentBuf strings.Builder
	var finishReason string
	var usage *deepseek.Usage
	var streamErr error

	for ev := range events {
		switch ev.Type {
		case deepseek.EventReasoningDelta:
			if !reasoningOpen {
				fmt.Println("== reasoning ==")
				reasoningOpen = true
			}
			fmt.Print(ev.Reasoning)
		case deepseek.EventContentDelta:
			if !contentOpen {
				if reasoningOpen {
					fmt.Println()
				}
				fmt.Println("\n== answer ==")
				contentOpen = true
			}
			fmt.Print(ev.Content)
			contentBuf.WriteString(ev.Content)
		case deepseek.EventToolCallDelta:
			// `ask` sends no tools, so this should never fire.
			fmt.Printf("\n[unexpected tool call delta: index=%d name=%s]\n", ev.ToolCall.Index, ev.ToolCall.Function.Name)
		case deepseek.EventFinish:
			finishReason = ev.FinishReason
		case deepseek.EventUsage:
			usage = ev.Usage
		case deepseek.EventError:
			streamErr = ev.Err
		}
	}
	elapsed := time.Since(start)
	fmt.Println()

	if streamErr != nil {
		return explainError(fmt.Errorf("stream: %w", streamErr))
	}

	if deepseek.IsReasoningStarved(finishReason, contentBuf.String()) {
		fmt.Fprintln(os.Stderr, "\nreasoning exhausted max_tokens before producing an answer; retry with a larger -max-tokens")
	}

	fmt.Println("\n== usage ==")
	if usage == nil {
		fmt.Println("no usage returned")
		return nil
	}

	reasoningTokens := 0
	if usage.CompletionTokensDetails != nil {
		reasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
	answerTokens := usage.CompletionTokens - reasoningTokens

	cost, costErr := priceTable.Cost(cfg.Model, usage.PromptCacheHitTokens, usage.PromptCacheMissTokens, usage.CompletionTokens)

	fmt.Printf("model          %s (effort %s, thinking %s)\n", cfg.Model, cfg.Effort, thinkingType)
	fmt.Printf("prompt tokens  %d (cache hit %d / cache miss %d, %s)\n", usage.PromptTokens, usage.PromptCacheHitTokens, usage.PromptCacheMissTokens, cacheHitRate(usage.PromptCacheHitTokens, usage.PromptCacheMissTokens))
	fmt.Printf("completion     %d (reasoning %d / answer %d)\n", usage.CompletionTokens, reasoningTokens, answerTokens)
	if costErr == nil {
		fmt.Printf("cost           $%.6f USD (price table captured %s)\n", cost, priceTable.CapturedAt)
	} else {
		fmt.Printf("cost           unknown: %v\n", costErr)
	}
	fmt.Printf("finish reason  %s\n", finishReason)
	fmt.Printf("wall clock     %s\n", elapsed.Round(time.Millisecond))
	return nil
}

func runModels(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	client := deepseek.NewClient(cfg.BaseURL, cfg.APIKey)

	resp, err := client.ListModels(ctx)
	if err != nil {
		return explainError(err)
	}
	for _, m := range resp.Data {
		fmt.Printf("%s (owned by %s)\n", m.ID, m.OwnedBy)
	}
	return nil
}

func runBalance(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("balance", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	client := deepseek.NewClient(cfg.BaseURL, cfg.APIKey)

	resp, err := client.GetBalance(ctx)
	if err != nil {
		return explainError(err)
	}
	fmt.Printf("available: %v\n", resp.IsAvailable)
	for _, b := range resp.BalanceInfos {
		fmt.Printf("%s  total=%s  granted=%s  topped_up=%s\n", b.Currency, b.TotalBalance, b.GrantedBalance, b.ToppedUpBalance)
	}
	return nil
}
