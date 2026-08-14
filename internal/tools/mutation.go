package tools

import "encoding/json"

// MutationTarget reports the workspace file a call to name would rewrite, and
// whether it rewrites one at all. Its purpose is upstream: the runner fans
// tool calls out into goroutines, and two calls that rewrite one file have to
// be ordered against each other rather than raced (internal/session,
// executeToolCalls). Everything else keeps running concurrently, so the
// question this answers is deliberately narrow — which file, if any.
//
// Edit and Write are the whole list. Both read a file, produce a new whole
// content, and write it back, so two of them on one path lose an edit no
// matter which wins the race; every other tool either only reads, or writes
// somewhere the model did not name. Bash is the honourable exception — it can
// rewrite anything — but its target is not knowable from its arguments, and
// pretending otherwise would serialise every Bash call against every Edit for
// no gain.
//
// The path is resolved the way the tools themselves resolve it, so two calls
// naming one file differently — "web/src/styles.css" and
// "./web/src/styles.css" — are recognised as the same file. Arguments that do
// not parse, name no file, or resolve outside the workspace report no target:
// such a call fails in execution anyway, and failing there gives the model the
// real reason instead of a scheduling artefact.
func MutationTarget(workspace, name, argsRaw string) (string, bool) {
	switch name {
	case "Edit", "Write":
	default:
		return "", false
	}
	var args struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(argsRaw), &args); err != nil || args.FilePath == "" {
		return "", false
	}
	path, err := ResolvePath(workspace, args.FilePath)
	if err != nil {
		return "", false
	}
	return path, true
}
