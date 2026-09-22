// Command harness hosts one coding session for a parent process over stdin
// and stdout, speaking the OpenAI Responses API's vocabulary
// (docs/STDIO-PROTOCOL.md). There is no server, no queue and no worker pool:
// the parent owns the working directory, supplies the credentials, and reads
// the session's events off the pipe.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mrgeoffrich/agent-harness/internal/provider"
)

const usage = `usage: harness <command> [flags]

commands:
  stdio-session     host one coding session for a parent process over stdin and stdout,
                    speaking the OpenAI Responses API's vocabulary
                    (docs/STDIO-PROTOCOL.md). Hosts every Gemini model this
                    binary routes, one DeepSeek model, and three Claude
                    models.
  gemini-session    the same session, spoken in Google's Interactions API
                    vocabulary instead (docs/STDIO-INTERACTIONS.md). Hosts
                    Google's models only.
  claude-session    the same session, spoken in Anthropic's Managed Agents
                    vocabulary instead (docs/STDIO-MANAGED-AGENTS.md). Hosts
                    the three Claude models only: claude-opus-5-5,
                    claude-sonnet-5, claude-fable-5-1.

The three commands differ in what the parent reads off the pipe, not in what
the session can do. Pick the one your client speaks.

"harness <command> -h" lists that command's flags.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	switch invoked := os.Args[1]; invoked {
	// The subcommand picks the parent-facing vocabulary, which is decided
	// before the handshake and cannot be negotiated after it: `initialize`
	// already has to answer with a protocol string, a capability named for
	// its own continuation id, and a model's effort set under its own key.
	//
	// gemini-session is the name this command was called when the only
	// models it hosted were Google's, and it spoke Interactions then. It
	// speaks Interactions again, so a client pinned at the last revision
	// that named it — c039c0b — works unchanged (docs/STDIO-INTERACTIONS.md).
	case "stdio-session", "gemini-session":
		if err := runStdioSession(ctx, invoked, os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "harness "+invoked+": "+err.Error())
			os.Exit(1)
		}
	case "claude-session":
		if err := runClaudeSession(ctx, os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "harness "+invoked+": "+err.Error())
			os.Exit(1)
		}
	case "-h", "-help", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown command %q\n\n%s\n", invoked, usage)
		os.Exit(2)
	}
}

// providerFor returns the provider that serves model. A name absent from the
// model→provider table is a caller typo or a retired model; the create
// handler rejects it before any of these call sites run, so here the
// DeepSeek fallback keeps the startup logs working on the default rather
// than failing on a stale setting (internal/provider,
// docs/KIMI-INTEGRATION.md §4.3).
func providerFor(model string) provider.Name {
	p, err := provider.ModelFor(model)
	if err != nil {
		return provider.DeepSeek
	}
	return p
}
