// Command drive runs one prompt through a harness session over the stdio
// protocol and prints the frames it reads back. It is a test client: the
// handshake, one create, wait for the run's terminal notification, close stdin.
// The exit status is 0 when the run completed, 1 when it failed or timed out.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type dialect struct {
	subcommand string
	create     string
	// body builds the create params from the flags.
	body func(o options) map[string]any
	// terminal reports whether a notification ends the run, and how it ended.
	terminal func(method string, params map[string]any) (done, ok bool, detail string)
}

type options struct {
	prompt, model, effort, cwd, mode string
}

var dialects = map[string]dialect{
	"claude": {
		subcommand: "claude-session",
		create:     "sessions.create",
		body: func(o options) map[string]any {
			model := map[string]any{"id": o.model}
			if o.effort != "" {
				model["effort"] = o.effort
			}
			return map[string]any{
				"agent": map[string]any{"type": "agent_with_overrides", "model": model},
				"initial_events": []any{map[string]any{
					"type":    "user.message",
					"content": []any{map[string]any{"type": "text", "text": o.prompt}},
				}},
				"harness": harnessBlock(o),
			}
		},
		terminal: func(method string, p map[string]any) (bool, bool, string) {
			if method != "session.status_idle" {
				return false, false, ""
			}
			reason, _ := dig(p, "harness", "reason").(string)
			return true, reason == "complete", reason
		},
	},
	"responses": {
		subcommand: "stdio-session",
		create:     "responses.create",
		body: func(o options) map[string]any {
			b := map[string]any{"input": o.prompt, "stream": true, "harness": harnessBlock(o)}
			if o.model != "" {
				b["model"] = o.model
			}
			if o.effort != "" {
				b["reasoning"] = map[string]any{"effort": o.effort}
			}
			return b
		},
		terminal: func(method string, p map[string]any) (bool, bool, string) {
			if method != "response.completed" {
				return false, false, ""
			}
			status, _ := dig(p, "response", "status").(string)
			return true, status == "completed", status
		},
	},
	"interactions": {
		subcommand: "gemini-session",
		create:     "interactions.create",
		body: func(o options) map[string]any {
			b := map[string]any{"input": o.prompt, "stream": true, "harness": harnessBlock(o)}
			if o.model != "" {
				b["model"] = o.model
			}
			if o.effort != "" {
				b["generation_config"] = map[string]any{"thinking_level": o.effort}
			}
			return b
		},
		terminal: func(method string, p map[string]any) (bool, bool, string) {
			if method != "interaction.completed" {
				return false, false, ""
			}
			status, _ := dig(p, "interaction", "status").(string)
			return true, status == "completed", status
		},
	},
}

func harnessBlock(o options) map[string]any {
	return map[string]any{"cwd": o.cwd, "permission_mode": o.mode}
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("drive", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: drive [flags] PROMPT")
		fs.PrintDefaults()
	}
	dname := fs.String("dialect", "claude", "vocabulary to speak: claude, responses or interactions")
	binary := fs.String("harness", "bin/harness", "path to the harness binary")
	model := fs.String("model", "", "model to run (claude defaults to claude-sonnet-5-5; the others to the harness's own default)")
	effort := fs.String("effort", "", "reasoning effort or thinking level")
	cwd := fs.String("cwd", "", "directory the session works in (default: a fresh temp dir)")
	mode := fs.String("mode", "readonly", "permission mode: readonly or full")
	envFile := fs.String("env", "", "KEY=VALUE file to pass to the harness as -env")
	stateDir := fs.String("state-dir", "", "state directory to pass to the harness, kept after the run")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up on the run after this long")
	width := fs.Int("width", 240, "truncate each printed frame to this many characters; 0 prints them whole")
	fs.Parse(os.Args[1:])

	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	d, ok := dialects[*dname]
	if !ok {
		fmt.Fprintf(os.Stderr, "drive: unknown dialect %q\n", *dname)
		return 2
	}
	if *dname == "claude" && *model == "" {
		*model = "claude-sonnet-5-5"
	}
	if *cwd == "" {
		dir, err := os.MkdirTemp("", "drive-cwd-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "drive:", err)
			return 1
		}
		defer os.RemoveAll(dir)
		*cwd = dir
	}

	args := []string{d.subcommand}
	if *envFile != "" {
		args = append(args, "-env", *envFile)
	}
	if *stateDir != "" {
		args = append(args, "-state-dir", *stateDir, "-keep-state")
	}
	cmd := exec.Command(*binary, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "drive:", err)
		return 1
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "drive:", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "drive:", err)
		return 1
	}

	send := func(v map[string]any) {
		v["jsonrpc"] = "2.0"
		line, _ := json.Marshal(v)
		fmt.Fprintf(stdin, "%s\n", line)
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"client_info": map[string]any{"name": "drive"}, "capabilities": map[string]any{},
	}})
	send(map[string]any{"method": "initialized", "params": map[string]any{}})
	send(map[string]any{"id": 2, "method": d.create, "params": d.body(options{
		prompt: fs.Arg(0), model: *model, effort: *effort, cwd: *cwd, mode: *mode,
	})})

	type frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
		Error  json.RawMessage `json:"error"`
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadString('\n')
			if line = strings.TrimRight(line, "\r\n"); line != "" {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()

	start := time.Now()
	deadline := time.After(*timeout)
	var cost float64
	var lastError string
	outcome, exit := "the process closed its output before the run finished", 1
loop:
	for {
		select {
		case <-deadline:
			outcome = fmt.Sprintf("timed out after %s", *timeout)
			break loop
		case line, open := <-lines:
			if !open {
				break loop
			}
			out := line
			if *width > 0 && len(out) > *width {
				out = out[:*width] + "…"
			}
			fmt.Println(out)

			var f frame
			if json.Unmarshal([]byte(line), &f) != nil {
				continue
			}
			if len(f.Error) > 0 && !bytes.Equal(f.Error, []byte("null")) {
				outcome = "the harness refused a request: " + string(f.Error)
				break loop
			}
			if c, ok := dig(f.Params, "usage", "harness", "cost_usd").(float64); ok {
				cost += c
			}
			if msg, ok := dig(f.Params, "error", "message").(string); ok {
				lastError = msg
			}
			if done, ok, detail := d.terminal(f.Method, f.Params); done {
				outcome = detail
				if !ok {
					outcome = strings.Join(strings.Fields("failed "+detail+" "+lastError), " ")
				}
				if ok {
					exit = 0
				}
				break loop
			}
		}
	}

	send(map[string]any{"id": 3, "method": "shutdown", "params": map[string]any{}})
	stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		<-exited
		fmt.Fprintln(os.Stderr, "drive: the harness did not exit after stdin closed; killed it")
	}

	summary := fmt.Sprintf("drive: %s in %s", outcome, time.Since(start).Round(100*time.Millisecond))
	if cost > 0 {
		summary += fmt.Sprintf(", cost $%.4f", cost)
	}
	fmt.Fprintln(os.Stderr, summary)
	return exit
}
