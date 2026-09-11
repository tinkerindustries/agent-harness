package responsesstdio

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden frame capture. One scripted run, every frame it produces,
// written out verbatim and compared byte for byte.
//
// The other tests in this package assert facts about frames — that a lifecycle
// is balanced, that the answer text arrives, that usage is on the terminal
// frame. None of them would notice a field that quietly changed name, moved
// between objects, or started being omitted, and all three are what a
// refactor of this package is most likely to do by accident. This one would.
//
// It is not a specification: docs/STDIO-PROTOCOL.md is that, and a change
// here that the document sanctions is a change to make. What the file buys is
// that no such change happens without somebody seeing it in a diff.

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden frame file from this run")

func TestGoldenFrames(t *testing.T) {
	f := newFixture(t,
		callThen("call-1", "mcp__host__echo", `{"text":"ping"}`),
		answer("The tool said pong."),
	)
	f.client.handshake(ClientCapabilities{FunctionCalls: true})

	f.client.mu.Lock()
	f.client.onRequest = func(m message) (any, *rpcError) {
		if m.Method != MethodFunctionCall {
			return nil, errorf(CodeMethodNotFound, "no %s", m.Method)
		}
		return FunctionCallResult{Output: TextPart(PartInputText, "pong")}, nil
	}
	f.client.mu.Unlock()

	params := f.createParams("use the tool")
	params.Tools = []Tool{{
		Type: ToolFunction, Name: "echo", Description: "Echo the text back",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Harness:    &ToolHarness{ReadOnly: true},
	}}

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, params, &created); rerr != nil {
		t.Fatalf("responses.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	// The assembled resource as responses.get answers with it, which is the
	// other document this protocol produces and is built by the same
	// translator.
	var got GetResult
	if rerr := f.client.call(MethodResponsesGet, IDParams{ResponseID: created.Response.ID}, &got); rerr != nil {
		t.Fatalf("responses.get: %v", rerr)
	}

	capture := map[string]any{
		"create_answer": normalise(t, created, f.cwd),
		"notifications": notificationFrames(t, seen, f.cwd),
		"get_answer":    normalise(t, got, f.cwd),
	}
	// An encoder rather than MarshalIndent, with HTML escaping off. The
	// default escapes the angle brackets on every placeholder below into
	// their \u form, and this file is meant to be read in a diff.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(capture); err != nil {
		t.Fatalf("encode the capture: %v", err)
	}
	out := buf.Bytes()

	path := filepath.Join("testdata", "responses-frames.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("make testdata: %v", err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run `go test ./internal/responsesstdio -run TestGoldenFrames -update-golden` to create it): %v", path, err)
	}
	if string(out) != string(want) {
		t.Errorf("the frames this run produced differ from %s.\n"+
			"If the change is one docs/STDIO-PROTOCOL.md sanctions, re-record with\n"+
			"  go test ./internal/responsesstdio -run TestGoldenFrames -update-golden\n"+
			"and put the diff in the commit. Otherwise it is a regression.\n\ngot:\n%s", path, out)
	}
}

// notificationFrames renders the stream as a list of {method, params}, with
// the params normalised.
func notificationFrames(t *testing.T, ms []message, cwd string) []any {
	t.Helper()
	out := make([]any, 0, len(ms))
	for _, m := range ms {
		var params any
		if len(m.Params) > 0 {
			if err := json.Unmarshal(m.Params, &params); err != nil {
				t.Fatalf("decode %s params: %v", m.Method, err)
			}
		}
		out = append(out, map[string]any{
			"method": m.Method,
			"params": scrub(params, cwd),
		})
	}
	return out
}

// normalise round-trips a value through JSON and scrubs it.
func normalise(t *testing.T, v any, cwd string) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return scrub(out, cwd)
}

// scrub replaces what differs between two runs of the same script: the
// minted response id and session id, both random; the two timestamps; and
// the working directory, which is a fresh temp directory each run and is
// quoted inside the opening message the model was given.
//
// Everything else — item ids, output indices, sequence numbers, call ids,
// usage, every piece of text — is a function of the script and is compared
// as it stands.
func scrub(v any, cwd string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch k {
			case "created_at":
				out[k] = "<created_at>"
			case "updated_at":
				out[k] = "<updated_at>"
			default:
				out[k] = scrub(val, cwd)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = scrub(val, cwd)
		}
		return out
	case string:
		if strings.HasPrefix(t, "resp_") {
			return "<response_id>"
		}
		if strings.HasPrefix(t, "sess-") {
			return "<session_id>"
		}
		if cwd != "" {
			t = strings.ReplaceAll(t, cwd, "<cwd>")
		}
		return t
	default:
		return v
	}
}
