# Guard assets

Copied from the classifier-research repository. The files are reference input for the guard's port. They are
not built or run here. Where a file here and the Go code disagree about the guard's decisions, the file wins
until classifier-research changes it. A change to a rule, prompt, cut-off or model id is made in
classifier-research first.

| File | Origin in classifier-research | Used by |
| --- | --- | --- |
| `stage1/prompt.yaml` | `evals/prompts/autonomy-stage1-scripts/baseline.yaml` | Stage 1 `questions`, sent as JSON |
| `stage1/experiment.yaml` | `evals/experiments/autonomy-stage1-scripts.yaml` | Stage 1 state fields |
| `stage1/model.yaml` | `evals/models/clef-flash.yaml`, without its `endpoint` line | Stage 1 model id |
| `stage2/AUTONOMY-STAGE2-PROMPT.md` | `docs/AUTONOMY-STAGE2-PROMPT.md` | Stage 2 system prompt, schema, request, state |
| `stage2/prompt.yaml` | `evals/prompts/autonomy-stage2-reasoning/baseline.yaml` | Source of the stage 2 system prompt |
| `stage2/experiment.yaml` | `evals/experiments/autonomy-stage2-reasoning.yaml` | Stage 2 state fields |
| `stage2/model.yaml` | `evals/models/haiku-5-5-medium.yaml` | Stage 2 model id and effort |
| `source/pipeline_report.py` | `src/pipeline_report.py` | Pipeline order and failure handling |
| `source/test_pipeline_report.py` | `tests/test_pipeline_report.py` | Pipeline cases, including the cut-off boundary |
| `source/guard_rules.py` | `src/guard_rules.py` | Rules |
| `source/script_detection.py` | `src/script_detection.py` | Scripts a command runs, escalation |
| `source/package_scripts.py` | `src/package_scripts.py` | Package scripts a command runs |

## Values not in a file above

- The stage 1 cut-off is 0.171. A command with p(block) at or above it goes to stage 2. It was chosen on the
  dev split of `autonomy-actions` by `src/stage1_threshold.py`, and the pipeline report is run with
  `--cutoff 0.171`.
- The stage 1 endpoint is supplied by the parent on `create`. The Ollama server's address is not in this
  repository.
- Stage 1 request: `POST <endpoint>/v1/systemone` with `model`, `keep_alive` (`30m`), `state` and
  `questions`. The state is the five stage 1 fields. The answer is
  `answers.<question>.choice` and `answers.<question>.probabilities`. It is valid only when the choice is one of
  the labels, the probabilities have exactly the two labels, each is a finite number in [0, 1], and they sum to
  1 within 0.01. Anything else is a stage 1 failure.
- Latency measured in the evals, as a guide for default deadlines: stage 1 about 150 ms per command, stage 2
  about 1.3 s median at medium effort. A deadline has to cover the transport's retries.
- Stage 2 `max_tokens` is 16000 and no temperature is sent.

## Not here

The parity fixtures for the rules, script detection and package scripts are exported separately by
classifier-research and land under `../parity/`. No holdout data is in this directory.
