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
import { FLASH_MODEL_KEY, listModels, resolveModelOptions, settingModel } from "../api/models";
import { listSettings } from "../api/settings";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { cn } from "@/lib/utils";

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
  // The model dropdown is fed by the harness, not a hardcoded copy: the
  // option list comes from GET /api/models — the provider table a work
  // request validates against — and the preselected model comes from the
  // model.flash settings row, whose default is the model this form used to
  // hardcode (docs/DATA-API.md "models"). When the models request fails,
  // resolveModelOptions falls back to the settings' two model defaults so
  // the dropdown is never empty.
  const [model, setModel] = useState("");
  const [models, setModels] = useState<string[]>([]);
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

  // The model dropdown is fed by the same lists the rest of the form is — the
  // server's own — so a browser can no more invent a model than a prompt
  // variant. The option list comes from GET /api/models and the preselection
  // from the model.flash settings row; when the models request fails the
  // dropdown falls back to the settings' two model defaults rather than
  // rendering empty (docs/DATA-API.md "models").
  useEffect(() => {
    let cancelled = false;
    Promise.allSettled([listModels(), listSettings()]).then(([modelsRes, settingsRes]) => {
      if (cancelled) return;
      const entries = settingsRes.status === "fulfilled" ? settingsRes.value : [];
      const flash = settingModel(entries, FLASH_MODEL_KEY);
      // The user cannot have picked anything yet — the select has no options
      // until this lands — so filling an empty selection is safe.
      setModel((prev) => (prev === "" ? flash : prev));
      setModels(
        resolveModelOptions(
          modelsRes.status === "fulfilled" ? modelsRes.value : null,
          entries,
        ),
      );
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
  // cleared Replicates field needs its own message, not the generic "pick
  // two variants".
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

  const fieldLabel = "text-xs tracking-[0.04em] text-muted-foreground uppercase";
  const fieldSelect = "bg-background text-foreground border border-border rounded-sm px-2 py-1.5 font-[inherit]";

  return (
    <div className="mb-8 flex max-w-[70ch] flex-col gap-4 rounded-lg border border-border p-6">
      <h2 className="m-0 text-base">Start an eval</h2>

      <label className="flex flex-col gap-1">
        <span className={fieldLabel}>Suite</span>
        <select className={fieldSelect} value={suite} onChange={(e) => setSuite(e.target.value)}>
          {suites.map((s) => (
            <option key={s.name} value={s.name}>
              {s.name} ({s.task_ids.length} tasks)
            </option>
          ))}
        </select>
      </label>
      {selectedSuite?.description && (
        <p className="block text-[0.85em] text-muted-foreground">{selectedSuite.description}</p>
      )}

      <div className="flex flex-col gap-1">
        <span className={fieldLabel}>Variants — the first is the baseline</span>
        <div className="flex flex-wrap gap-1">
          {variants.map((v) => {
            const at = chosen.indexOf(v.name);
            return (
              <button
                key={v.name}
                type="button"
                className={cn(
                  "inline-flex cursor-pointer items-center gap-1 rounded-sm border border-border bg-transparent px-2.5 py-1 font-[inherit] text-inherit",
                  at >= 0 && "border-primary bg-accent",
                )}
                onClick={() => toggleVariant(v.name)}
                title={v.description}
              >
                {/* The ordinal, not the word "baseline": one of the variants
                    is itself named base, and the two read as a stutter. */}
                {at >= 0 && (
                  <span className="inline-flex h-[1.3em] min-w-[1.3em] items-center justify-center rounded-full bg-primary text-[0.75em] text-primary-foreground tabular-nums">
                    {at + 1}
                  </span>
                )}
                {v.name}
              </button>
            );
          })}
        </div>
      </div>

      <div className="flex flex-wrap gap-4">
        <label className="flex flex-[1_1_18ch] min-w-[18ch] flex-col gap-1">
          <span className={fieldLabel}>Replicates</span>
          <Input className="min-w-0" value={replicates} onChange={(e) => setReplicates(e.target.value)} inputMode="numeric" />
        </label>
        <label className="flex flex-[1_1_18ch] min-w-[18ch] flex-col gap-1">
          <span className={fieldLabel}>Model</span>
          <select className={fieldSelect} value={model} onChange={(e) => setModel(e.target.value)}>
            {models.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-[1_1_18ch] min-w-[18ch] flex-col gap-1">
          <span className={fieldLabel}>Max sub-turns</span>
          <Input
            className="min-w-0"
            value={maxSubTurns}
            onChange={(e) => setMaxSubTurns(e.target.value)}
            inputMode="numeric"
            placeholder="suite default"
          />
        </label>
      </div>

      <label className="flex flex-col gap-1">
        <span className={fieldLabel}>Note</span>
        <Input value={note} onChange={(e) => setNote(e.target.value)} placeholder="what is this run asking?" />
      </label>

      <label className="flex items-center gap-1">
        <input type="checkbox" checked={judge} onChange={(e) => setJudge(e.target.checked)} />
        <span>Judge each transcript with a model as well as the counters</span>
      </label>

      <p className="m-0 text-muted-foreground">
        {blocker ?? (
          <>
            <strong>{totalRuns} runs</strong>
            {estimate !== null && <> · roughly ${estimate.toFixed(2)}, from earlier {suite} runs</>}
            {estimate === null && <> · no earlier {suite} runs to estimate a cost from</>}
          </>
        )}
      </p>

      {error && <p className="text-destructive">{error}</p>}

      <div className="flex gap-1">
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
