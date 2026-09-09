package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/config"
	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/responsesstdio"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// defaultGeminiSessionModel is what a create body with no `model` runs on
// when this process was given a Google key.
const defaultGeminiSessionModel = "gemini-3.7-flash"

// deepSeekSessionModel is the one DeepSeek model this command hosts, and the
// only one it will host: it is the only DeepSeek model that reads images
// natively (provider.SeesImages, docs/DEEPSEEK-VISION.md), which is what
// makes a DeepSeek session here need one credential rather than two.
//
// The other two — deepseek-v4-flash and deepseek-v4-pro — are deliberately
// not offered. A session on either gets the tool array built for a model
// that cannot see (Screenshot, Glance, Ground, Detect, Transcribe, Crop),
// and four of those six send their images to Google, so hosting them would
// mean a DeepSeek run silently needing a Google API key as well or carrying
// six tools that fail whenever the model reaches for them. Nothing about the
// rest of the harness changes: `harness serve` routes all three as before.
const deepSeekSessionModel = "deepseek-v4-flash-vision-exp"

// runStdioSession hosts one coding session for a parent process over in and
// out, speaking the protocol docs/STDIO-PROTOCOL.md describes. main.go calls
// it with os.Stdin and os.Stdout; a test calls it with a pipe, so the
// process's whole setup — credential handling among it — runs without a
// child process to launch or a real stdin to close.
//
// invoked is the subcommand the process was actually spawned as —
// `stdio-session`, or the `gemini-session` alias it was called before it
// hosted a model that is not Google's. It is threaded through rather than
// hardcoded so that a parent pinning server_info.name is told the name it
// used, which is what makes the alias a compatible one rather than a
// redirect that changes the handshake under a client that has not moved yet.
//
// Nothing may reach out but protocol frames. Go's log package writes to
// stderr already; this pins it, because a stray line on stdout would be an
// unparseable frame to the parent and there is no recovering from that.
func runStdioSession(ctx context.Context, invoked string, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet(invoked, flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "directory for this session's own SQLite state and transcript mirror (default: a per-process directory under the user cache dir)")
	keepState := fs.Bool("keep-state", false, "leave the state directory behind when the process exits, for reading a finished session's transcript")
	prices := fs.String("prices", "configs/prices.json", "price table, for the cost figures reported on harness.usage")
	model := fs.String("model", "", "model a create body with no `model` runs on (default gemini-3.7-flash, or "+deepSeekSessionModel+" when only a DeepSeek key was supplied)")
	envFile := fs.String("env", "", "read the API keys from this KEY=VALUE `file` when the environment does not carry them, for driving the process by hand")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log.SetOutput(os.Stderr)
	log.SetPrefix("harness " + invoked + ": ")

	// The keys arrive in the environment the parent spawned this process
	// with. There is no settings store to read one from and no screen to
	// type one into: a hosted session's credentials are the host's to
	// supply. GOOGLE_API_KEY is accepted as well because that is the name
	// Google's own SDKs read. -env names a file to fall back to, for a
	// person driving the process by hand rather than a parent application.
	//
	// Both providers' keys are read, and neither is required: a host with
	// one key runs that provider's models and is told which variable is
	// missing if it asks for the other's (responsesstdio's missingKeyMessage).
	apiKey, deepSeekKey, err := apiKeys(*envFile)
	if err != nil {
		return err
	}

	chosen, err := resolveHostedModel(*model, apiKey, deepSeekKey)
	if err != nil {
		return err
	}

	dir := *stateDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "agent-harness", "stdio-session", fmt.Sprint(os.Getpid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	if !*keepState && *stateDir == "" {
		defer os.RemoveAll(dir)
	}

	// The agent loop's state machine is its event log, and the log lives in
	// SQLite (internal/session, internal/store). This is that log and
	// nothing else: no work queue is served, no worker pool claims from it,
	// no HTTP listener reads it. docs/STDIO-PROTOCOL.md records why the
	// binary has a database at all.
	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	// The key reaches the Gemini API client directly, as a closure over this
	// local variable, and never becomes a settings-table row: a -state-dir
	// this process's parent keeps for resuming must not be a file holding
	// the plaintext key (docs/STDIO-PROTOCOL.md, "Trust boundaries";
	// cmd/harness/serve.go's own use of settings.KeyGoogleAPIKey is a
	// different mode — an operator's key, typed into that harness's own
	// settings screen, with a different lifetime — and is unaffected).
	res := settings.NewResolver(st)

	priceTable, err := pricing.Load(*prices)
	if err != nil {
		// A missing price table costs the cost figure on harness.usage and
		// nothing else, so it is a warning rather than a refusal to start.
		log.Printf("no price table (%v); harness.usage will report tokens without a cost", err)
		priceTable = nil
	}

	geminiClient := gemini.NewClient(gemini.DefaultBaseURL, gemini.WithAPIKeyProvider(func() (string, error) {
		return apiKey, nil
	}))
	// The Responses API, not Chat Completions: this entry point speaks that
	// vocabulary to its parent, and speaking it to the provider too means
	// one shape end to end (docs/DEEPSEEK-RESPONSES.md). `harness serve` is
	// deliberately unchanged and still posts /chat/completions — a session
	// speaks one surface for its whole life, so switching that one is a
	// decision about its running sessions rather than a wiring change.
	deepSeekClient := deepseek.NewResponsesClient(deepseek.DefaultBaseURL, "", deepseek.WithAPIKeyProvider(func() (string, error) {
		return deepSeekKey, nil
	}))
	// The composition point the architecture names: one client per provider,
	// and one place that decides which of them a model resolves to. No Kimi
	// client, because no Kimi model is hosted here.
	clientFor := func(m string) session.Client {
		if providerFor(m) == provider.DeepSeek {
			return deepSeekClient
		}
		return geminiClient
	}

	mcpMgr := mcpclient.New(st)
	mcpMgr.EnvFilter = stripProviderAPIKeys
	defer mcpMgr.Close()

	eventHub := hub.New()
	runner := &session.Runner{
		Store:     st,
		Mirror:    store.NewMirror(dir),
		Client:    clientFor(chosen),
		ClientFor: clientFor,
		Prices:    priceTable,
		Gemini:    geminiClient,
		// The flash model is what WebFetch summarises with and what
		// compaction runs on, so it follows the session's own model rather
		// than being pinned to one provider — a DeepSeek session that
		// summarised a page through Google would need the key the whole
		// point of hosting this model is not needing.
		FlashModel: chosen,
		Hub:        eventHub,
		Settings:   res,
		// The vision tools this resolves the model for (Glance, Ground,
		// Detect) are Google's whatever the session runs on, so it stays a
		// Gemini name even when the session's own model is DeepSeek's. No
		// model hosted here is offered those tools at all — every one of
		// them reads images natively, which is why the DeepSeek model
		// hosted here is the vision one — so this is what it would have to
		// be if that ever changed rather than something in use today.
		GeminiModel:   func() (string, error) { return geminiVisionModel(chosen), nil },
		ToolEnvFilter: stripProviderAPIKeys,
	}

	srv := responsesstdio.NewServer(responsesstdio.Options{
		Store:        st,
		Runner:       runner,
		Hub:          eventHub,
		MCP:          mcpMgr,
		Models:       hostedModels(),
		DefaultModel: chosen,
		ServerName:   "agent-harness " + invoked,
		HasAPIKey: func(m string) bool {
			if providerFor(m) == provider.DeepSeek {
				return deepSeekKey != ""
			}
			return apiKey != ""
		},
		Version: buildVersion(),
	})

	log.Printf("ready; default model %s, state under %s", chosen, dir)
	return srv.Serve(ctx, in, out)
}

// hostedModels is the model list the handshake advertises, and the list a
// create's `model` is checked against: every model internal/provider routes
// to Google, then the one DeepSeek model this command hosts.
//
// It is not "every model internal/provider knows". The repository routes
// three DeepSeek models and one Kimi model this process does not offer —
// Kimi because no client is built for it here, and DeepSeek's other two
// because they cannot see images (deepSeekSessionModel). A create naming any
// of them is refused by name against this list.
func hostedModels() []string {
	var out []string
	for _, m := range provider.KnownModels() {
		if p, err := provider.ModelFor(m); err == nil && p == provider.Gemini {
			out = append(out, m)
		}
	}
	return append(out, deepSeekSessionModel)
}

// resolveHostedModel decides what a create body with no `model` runs on.
//
// A -model the parent named is taken as given and checked against the hosted
// list, so a name this process would refuse at create is refused at startup
// instead — the parent hears it on the pipe it just spawned rather than on
// its first interaction. Named nothing, the default follows the credentials:
// Google's model normally, DeepSeek's when a DeepSeek key was supplied and a
// Google one was not, because a host with one key meant the model that key
// runs. With neither key the Google default stands and the first create
// fails with -32003 naming the variables, which is the documented behaviour
// for a process started without credentials (docs/STDIO-PROTOCOL.md).
func resolveHostedModel(named, googleKey, deepSeekKey string) (string, error) {
	if named == "" {
		if googleKey == "" && deepSeekKey != "" {
			return deepSeekSessionModel, nil
		}
		return defaultGeminiSessionModel, nil
	}
	for _, m := range hostedModels() {
		if m == named {
			return named, nil
		}
	}
	return "", fmt.Errorf("-model %s: this process hosts %s", named, strings.Join(hostedModels(), ", "))
}

// geminiVisionModel is the Gemini model the vision tools send images to when
// the session itself runs on one this process hosts. Google's models answer
// for themselves; a DeepSeek session falls back to the Gemini default,
// because those tools reach Google whatever the session's own model is.
func geminiVisionModel(sessionModel string) string {
	if providerFor(sessionModel) == provider.Gemini {
		return sessionModel
	}
	return defaultGeminiSessionModel
}

// stampedVersion is set at link time with -ldflags "-X main.stampedVersion=vX.Y.Z"
// and is empty in every build that does not pass it.
//
// A parent that packages this binary pins the exact version it expects the
// handshake to report and refuses one that reports anything else, and no
// build made from a source checkout can satisfy that on its own: Go stamps
// debug.ReadBuildInfo().Main.Version from the module proxy, so `go build`
// reports "(devel)" even from a clean tree at the exact tag, and `go install
// path@version` reports a pseudo-version for any commit that is not itself
// tagged. The release build passes the tag here.
var stampedVersion string

// buildVersion is what the handshake reports as this process's version: the
// link-time stamp when a release build set one, otherwise the module build
// info, which is "(devel)" for a local build.
func buildVersion() string {
	if stampedVersion != "" {
		return stampedVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "unknown"
	}
	return info.Main.Version
}

// apiKeys resolves both providers' keys: the environment first, then the
// file -env names, if it named one. Either may come back empty — a host that
// supplies one key runs that provider's models — and the file is read once
// for both.
func apiKeys(envFile string) (google, deepSeek string, err error) {
	google = firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY"))
	deepSeek = os.Getenv("DEEPSEEK_API_KEY")
	if envFile == "" {
		return google, deepSeek, nil
	}
	values, err := config.DotEnvValues(envFile)
	if err != nil {
		return "", "", fmt.Errorf("-env %s: %w", envFile, err)
	}
	return firstNonEmpty(google, values["GEMINI_API_KEY"], values["GOOGLE_API_KEY"]),
		firstNonEmpty(deepSeek, values["DEEPSEEK_API_KEY"]),
		nil
}

// providerAPIKeyVars are the environment variables this process reads a
// model provider's credential from (apiKeys). Every one of them is stripped
// from what a session's tools inherit.
var providerAPIKeyVars = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "DEEPSEEK_API_KEY"}

// stripProviderAPIKeys removes every variable in providerAPIKeyVars from
// base, which every Bash call and every stdio MCP dial this hosted process
// makes takes as its environment's starting point (tools.Executor.EnvFilter,
// mcpclient.Manager.EnvFilter). All the names a key is read under are
// stripped, not just the one a given run used: a session tool seeing the raw
// variable this process itself was handed would defeat the reason the key
// never reaches the settings table either. DEEPSEEK_API_KEY joined the list
// when this command began hosting a DeepSeek model, and it matters at least
// as much as the other two — a session working in this very repository has a
// harness of its own that reads that variable.
func stripProviderAPIKeys(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if hasAnyPrefix(kv, providerAPIKeyVars) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasAnyPrefix reports whether kv is an assignment to one of names.
func hasAnyPrefix(kv string, names []string) bool {
	for _, n := range names {
		if strings.HasPrefix(kv, n+"=") {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
