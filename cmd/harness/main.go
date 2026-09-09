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
                    speaking the OpenAI Responses API's vocabulary (docs/STDIO-PROTOCOL.md).
                    gemini-session is the former name and still works

"harness <command> -h" lists that command's flags.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	switch invoked := os.Args[1]; invoked {
	// gemini-session is what this command was called when the only models it
	// hosted were Google's, and it is kept as an alias: a parent that spawns
	// the old name gets the same process, and the handshake reports back the
	// name it was actually spawned as, so a client pinning server_info.name
	// keeps working until it has moved (docs/STDIO-PROTOCOL.md, "Starting
	// the process").
	case "stdio-session", "gemini-session":
		if err := runStdioSession(ctx, invoked, os.Args[2:], os.Stdin, os.Stdout); err != nil {
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
