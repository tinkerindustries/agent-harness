package tools

import (
	"encoding/json"
	"strings"
)

// MutationTargets reports the workspace files a call to name would rewrite,
// and nil when it rewrites none. Its purpose is upstream: the runner fans
// tool calls out into goroutines, and two calls that rewrite one file have to
// be ordered against each other rather than raced (internal/session,
// executeToolCalls). Everything else keeps running concurrently, so the
// question this answers is deliberately narrow: which files, if any.
//
// `edit` and `write` rewrite one file each. apply_patch can rewrite any number,
// so it reports every path its patch names, a move's destination included:
// the caller orders the call against each of them, and a call naming several
// is what makes the grouping more than a single path. Both read a file,
// produce a new whole content, and write it back, so two of them on one path
// lose an edit no matter which wins the race; every other tool either only
// reads, or writes somewhere the model did not name. `bash` is the honourable
// exception: it can rewrite anything, but its target is not knowable from its
// arguments, and pretending otherwise would serialise every `bash` call against
// every `edit` for no gain.
//
// The paths are resolved the way the tools themselves resolve them, so two
// calls naming one file differently ("web/src/styles.css" and
// "./web/src/styles.css") are recognised as the same file. Arguments that
// do not parse, name no file, or resolve outside the workspace report no
// target: such a call fails in execution anyway, and failing there gives the
// model the real reason instead of a scheduling artefact. A patch with one
// path that escapes the workspace reports no target at all for the same
// reason: the whole call is going to be refused.
func MutationTargets(workspace, name, argsRaw string) []string {
	switch name {
	case "edit", "write":
		var args struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal([]byte(argsRaw), &args); err != nil || args.FilePath == "" {
			return nil
		}
		path, err := ResolvePath(workspace, args.FilePath)
		if err != nil {
			return nil
		}
		return []string{path}

	case "apply_patch":
		var args applyPatchArgs
		if err := json.Unmarshal([]byte(argsRaw), &args); err != nil || args.Input == "" {
			return nil
		}
		ops, err := parsePatch(args.Input)
		if err != nil {
			return nil
		}
		var targets []string
		seen := make(map[string]bool, len(ops))
		for _, op := range ops {
			for _, display := range []string{op.path, op.moveTo} {
				if display == "" {
					continue
				}
				path, err := ResolvePath(workspace, display)
				if err != nil {
					return nil
				}
				if !seen[path] {
					seen[path] = true
					targets = append(targets, path)
				}
			}
		}
		return targets
	}
	return nil
}

// CreatedTargets reports which of the files MutationTargets names a
// successful call to name made, as opposed to rewrote. A `write` says so in
// its result ("Created <path>" against "Overwrote <path>"), so result is the
// result content of the call. A patch's Add File operations and the
// destination of a move create files; an update does not. `edit` never
// creates.
func CreatedTargets(workspace, name, argsRaw, result string) []string {
	switch name {
	case "write":
		if !strings.HasPrefix(result, "Created ") {
			return nil
		}
		return MutationTargets(workspace, name, argsRaw)

	case "apply_patch":
		var args applyPatchArgs
		if err := json.Unmarshal([]byte(argsRaw), &args); err != nil || args.Input == "" {
			return nil
		}
		ops, err := parsePatch(args.Input)
		if err != nil {
			return nil
		}
		var created []string
		for _, op := range ops {
			display := op.moveTo
			if op.kind == patchAdd {
				display = op.path
			}
			if display == "" {
				continue
			}
			if path, err := ResolvePath(workspace, display); err == nil {
				created = append(created, path)
			}
		}
		return created
	}
	return nil
}
