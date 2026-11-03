# Guard parity fixtures

Inputs and outputs of the Python guard functions, for proving a port against them.
`src/export_guard_fixtures.py` writes two sets in the same format:

```
.venv/bin/python src/export_guard_fixtures.py [--out parity-out]
.venv/bin/python src/export_guard_fixtures.py --synthetic [--out parity-out/synthetic]
```

- The first set runs the functions over every case in a dataset of real commands (named in the
  exporter's docstring). It holds real commands and paths and is not for publishing.
- The synthetic set runs them over the invented cases in `tests/data/guard-parity-synthetic.yaml`. It is built to
  be published, and covers variants the dataset does not exercise. See [Synthetic set](#synthetic-set).

The commands are inert text and are never executed. The files are produced by calling `src/script_detection.py` and `src/package_scripts.py` unchanged. The exporter reads no held-out data. The
exporter is deterministic: two runs write identical bytes.

## Format

Each `*.jsonl` file has one JSON object per line, one line per case, ordered by `id`. Keys are sorted. The text is
ASCII: non-ASCII characters are written as `\uXXXX` escapes. Strings keep their newlines, quotes and spacing
exactly. Every `id` appears once in every file, including cases whose `expected` is empty.

`expected` is what the function returned, serialised once. If the function raised, `expected` is
`{"error": "<exception type>", "message": "<text>"}`.

## Files

### `executed_scripts.jsonl`: `script_detection.executed_scripts(command)`

| Field | Type | Meaning |
| --- | --- | --- |
| `id`, `command` | string | As above. |
| `expected` | array of string | Script paths exactly as written in the command. |

The list has no repeats. The order is the order of first appearance in the command. It is not sorted.

### `escalation_reasons.jsonl`: `script_detection.escalation_reasons(command, scripts_run)`

| Field | Type | Meaning |
| --- | --- | --- |
| `id`, `command` | string | As above. |
| `scripts_run` | array of object | The case's `scripts_run` state field, as the evals pass it. |
| `expected` | array of string | Why the command skips stage 1. |

Each `scripts_run` entry has `path` (string), `origin` (`existing`, `written_this_session` or
`changed_this_session`) and `content` (string or null). `expected` holds `runs_several_scripts` then
`unscreened_script`, in that order, each at most once.

### `package_scripts.jsonl`: `package_scripts.resolve(command, working_directory, repositories)`

| Field | Type | Meaning |
| --- | --- | --- |
| `id`, `command` | string | As above. |
| `working_directory` | string | The case's `working_directory`. |
| `repositories` | array of string | One element: the working directory up to `/.claude/worktrees/`, or the working directory itself when it has none (`annotate_package_scripts.repository_of`). |
| `expected` | array of object | What `resolve` returned. |
| `files` | object | Path to filesystem state, for every path `resolve` asked about. |

Each `expected` entry has `runner` (string), `script` (string), `command` (string, or null when package.json has no
such script) and `files` (array of `{path, excerpt, truncated}`; `excerpt` is a string or null, `truncated` a
boolean). The order is the order `resolve` visited them: depth first, at most 8 entries. It is not sorted.

`files` maps each path string, exactly as `resolve` passed it to the filesystem, to one of:

- a string: the file's text, read as UTF-8 with newlines translated to `\n`.
- `null`: the path is neither a file nor a directory.
- `{"directory": true}`: the path is a directory.
- `{"unreadable": true}`: the path is a file that could not be read as UTF-8.

The paths are absolute. Directories hold no listing because `resolve` only tests whether a path is a directory.
Python's `Path.resolve()` is applied to script paths before they are read. The keys hold the result of lexical
normalisation (`.` and `..` collapsed, no symlinks followed), and the exporter replays `resolve` with exactly
that normalisation.

The exporter checks every case: it runs `resolve` again against `files` alone, with the filesystem calls
replaced, and stops with an error if the result differs from `expected` or if `resolve` asks about a path the
snapshot lacks.

## Python behaviours a port has to match

- `shlex.split(segment, comments=False)`, falling back to `str.split()` when it raises `ValueError` (an unbalanced
  quote). The command is split into segments with `_SPLIT` (`src/script_detection.py`) before this, so a quoted
  string that contains `;` or `|` is often cut in the middle of the quote.
- `re` features that RE2 lacks: the lookbehinds in `_START` and `_QUOTED` and the backreferences in the heredoc pattern. `\s`, `\w`, `str.split()` and `str.strip()` are Unicode-aware.

## Synthetic set

The inputs are in `tests/data/guard-parity-synthetic.yaml`. Every command, path, host and file text is invented.
Paths sit under `/work`. Nothing is taken from the dataset. `expected` is whatever the Python functions return, so
a case that documents a quirk records the quirk.

Differences from the dataset set:

- Each file has its own list of cases. An `id` appears in one file only, and ids describe the case.
- `branch` is null where the case gives none. `scripts_run` is null where the case gives none.
- `repositories` is the list the case gives, and may be empty or hold several paths.
- No function raises on any synthetic case, so `expected` never holds an error object.

For `package_scripts.jsonl` the exporter builds the case's files in a temporary directory, runs `resolve` there
through the same recorder, then rewrites the temporary prefix to `/work` everywhere. The package.json probes
above the temporary directory are dropped and `/package.json` (null) stands for them. The published row is then
replayed against its own snapshot, and the exporter stops with an error if the replay differs.

Snapshot keys are the strings `resolve` built, so a key can hold `..` when a command passes a relative directory
after a `cd` (for example `/work/app/packages/web/../api/package.json`). A port that cleans paths before looking
them up needs to look up the same strings.
