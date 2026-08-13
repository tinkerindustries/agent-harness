import { useEffect, useMemo, useState } from "react";

import {
  estimateCost,
  listEvalSuites,
  listEvalVariants,
  startEval,
  type EvalRunRow,
  type EvalSuiteRow,
  type EvalVariantRow,
} from "../api/evals";
import { Button } from "./ui/button";
import { Input } from "./ui/input";

// The start form (docs/EVALS.md): a card above the eval list, following the
// start-run form's shape. It names only suites and variants this build
// already has — the pickers are fed by the server's own lists — so there is
// no free-text prompt field and no way for a browser to introduce a system
// prompt the build does not know. That is the docs/CACHE.md constraint
// holding by construction rather than by discipline.
//
// A start spends real money, so the confirm states the run count and an
// estimate first.

interface Props {
  token: string;
  priorRuns: EvalRunRow[];
  onClose: () => void;
  onStarted: (id: string) => void;
}

export function EvalStartForm({ token, priorRuns, onClose, onStarted }: Props) {
  const [suites, setSuites] = useState<EvalSuiteRow[]>([]);
  const [variants, setVariants] = useState<EvalVariantRow[]>([]);
  const [suite, setSuite] = useState("");
  const [chosen, setChosen] = useState<string[]>([]);
  const [replicates, setReplicates] = useState("3");
  const [model, setModel] = useState("deepseek-v4-flash");
  const [maxSubTurns, setMaxSubTurns] = useState("");
  const [judge, setJudge] = useState(true);
  const [note, setNote] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    Promise.all([listEvalSuites(), listEvalVariants()])
      .then(([s, v]) => {
        if (cancelled) return;
        setSuites(s);
        setVariants(v);
        if (s.length > 0) setSuite(s[0].name);
        // Baseline first, which is what the delta compares against.
        setChosen(v.length > 1 ? [v[0].name, v[1].name] : v.map((x) => x.name));
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const selectedSuite = suites.find((s) => s.name === suite);
  // A fractional value is rejected rather than truncated: 2.5 silently
  // becoming 2 changes what the run measures without saying so.
  const reps = /^\d+$/.test(replicates.trim()) ? Number.parseInt(replicates, 10) : Number.NaN;
  const totalRuns =
    selectedSuite && Number.isFinite(reps) && reps > 0 ? selectedSuite.task_ids.length * chosen.length * reps : 0;
  const estimate = useMemo(() => estimateCost(totalRuns, suite, priorRuns), [totalRuns, suite, priorRuns]);

  // Two variants at least, and the first is the baseline. Toggling keeps the
  // click order, so the operator chooses which arm the delta is measured
  // against by picking it first.
  function toggleVariant(name: string) {
    setChosen((prev) => (prev.includes(name) ? prev.filter((v) => v !== name) : [...prev, name]));
  }

  // Say which condition is unmet rather than one message for all of them: a
  // cleared Replicates field used to report "pick two variants".
  const blocker =
    suite === ""
      ? "Pick a suite."
      : chosen.length < 2
        ? "Pick at least two variants — the comparison needs a baseline and an arm."
        : !Number.isFinite(reps) || reps < 1
          ? "Replicates must be a whole number, at least 1."
          : null;
  const ready = blocker === null && !sending;

  async function submit() {
    setSending(true);
    setError(null);
    try {
      const id = await startEval(token, {
        suite,
        variants: chosen,
        replicates: reps,
        model: model || undefined,
        max_sub_turns: maxSubTurns ? Number.parseInt(maxSubTurns, 10) : undefined,
        judge,
        note: note || undefined,
      });
      onStarted(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSending(false);
    }
  }

  return (
    <div className="eval-start">
      <h2>Start an eval</h2>

      <label className="eval-field">
        <span>Suite</span>
        <select value={suite} onChange={(e) => setSuite(e.target.value)}>
          {suites.map((s) => (
            <option key={s.name} value={s.name}>
              {s.name} ({s.task_ids.length} tasks)
            </option>
          ))}
        </select>
      </label>
      {selectedSuite?.description && <p className="eval-note">{selectedSuite.description}</p>}

      <div className="eval-field">
        <span>Variants — the first is the baseline</span>
        <div className="eval-variant-picker">
          {variants.map((v) => {
            const at = chosen.indexOf(v.name);
            return (
              <button
                key={v.name}
                type="button"
                className={at >= 0 ? "eval-variant-chip is-on" : "eval-variant-chip"}
                onClick={() => toggleVariant(v.name)}
                title={v.description}
              >
                {/* The ordinal, not the word "baseline": one of the variants
                    is itself named base, and the two read as a stutter. */}
                {at >= 0 && <span className="eval-variant-order">{at + 1}</span>}
                {v.name}
              </button>
            );
          })}
        </div>
      </div>

      <div className="eval-field-row">
        <label className="eval-field">
          <span>Replicates</span>
          <Input value={replicates} onChange={(e) => setReplicates(e.target.value)} inputMode="numeric" />
        </label>
        <label className="eval-field">
          <span>Model</span>
          <Input value={model} onChange={(e) => setModel(e.target.value)} />
        </label>
        <label className="eval-field">
          <span>Max sub-turns</span>
          <Input
            value={maxSubTurns}
            onChange={(e) => setMaxSubTurns(e.target.value)}
            inputMode="numeric"
            placeholder="suite default"
          />
        </label>
      </div>

      <label className="eval-field">
        <span>Note</span>
        <Input value={note} onChange={(e) => setNote(e.target.value)} placeholder="what is this run asking?" />
      </label>

      <label className="eval-checkbox">
        <input type="checkbox" checked={judge} onChange={(e) => setJudge(e.target.checked)} />
        <span>Judge each transcript with a model as well as the counters</span>
      </label>

      <p className="eval-cost">
        {blocker ?? (
          <>
            <strong>{totalRuns} runs</strong>
            {estimate !== null && <> · roughly ${estimate.toFixed(2)}, from earlier {suite} runs</>}
            {estimate === null && <> · no earlier {suite} runs to estimate a cost from</>}
          </>
        )}
      </p>

      {error && <p className="eval-error">{error}</p>}

      <div className="eval-actions">
        <Button onClick={() => void submit()} disabled={!ready}>
          {sending ? "Starting…" : `Start ${totalRuns} runs`}
        </Button>
        <Button variant="ghost" onClick={onClose} disabled={sending}>
          Cancel
        </Button>
      </div>
    </div>
  );
}
