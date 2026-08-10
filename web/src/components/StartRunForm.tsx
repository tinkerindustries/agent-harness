import { useState, useSyncExternalStore } from "react";
import { errorMessage, parseRepoSpec, startRun, type WorkRequest } from "../api/operations";
import { sessionListStore } from "../api/sessionListStore";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "./ui/collapsible";
import { ToggleGroup, ToggleGroupItem } from "./ui/toggle-group";

// StartRunForm is the browser's start form (docs/RUN-CONTROL.md "The
// frontend"): prompt, repos (URL#branch, repeatable), an explicit permission
// mode, and the optional fields behind a disclosure so the common case stays
// short. Submitting POSTs the work request and gets a request_id back; the
// session itself appears on the session-list feed once the pool claims it,
// and this form follows that feed — it never polls and never invents a
// placeholder row. The permission mode carries the one warning this surface
// must not bury: full runs as root in the workspace, and the harness
// container has the host's docker socket, so a full run has the host daemon.
//
// The write is an acceptance, not an outcome: the 202 only means the request
// landed on the WORK stream. The operator's token is passed in from the
// screen, which fetched it via controlToken() and refuses to show this form
// at all when run control is not configured (a null token would only 503).
interface StartRunFormProps {
  token: string;
  onClose: () => void;
  onOpen: (id: string) => void;
}

export function StartRunForm({ token, onClose, onOpen }: StartRunFormProps) {
  const [prompt, setPrompt] = useState("");
  const [repoSpecs, setRepoSpecs] = useState<string[]>([""]);
  const [permission, setPermission] = useState("");
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  const [deny, setDeny] = useState("");
  const [resultSchema, setResultSchema] = useState("");
  const [maxSubTurns, setMaxSubTurns] = useState("");
  const [deadlineMs, setDeadlineMs] = useState("");
  const [jobType, setJobType] = useState("");
  const [parentAgentType, setParentAgentType] = useState("");
  const [parentAgentID, setParentAgentID] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [accepted, setAccepted] = useState<string | null>(null);

  // The screen follows the new session from the list feed the screen is
  // already connected to: a session whose request_id matches what the 202
  // named is the one this form started. No polling, no placeholder row —
  // the feed is what says a session started.
  const snapshot = useSyncExternalStore(sessionListStore.subscribe, sessionListStore.getSnapshot);
  const started = accepted ? snapshot.sessions.find((s) => s.request_id === accepted) : undefined;

  const setRepoAt = (i: number, v: string) => {
    setRepoSpecs((prev) => prev.map((spec, j) => (j === i ? v : spec)));
  };

  const reset = () => {
    setPrompt("");
    setRepoSpecs([""]);
    setPermission("");
    setModel("");
    setEffort("");
    setDeny("");
    setResultSchema("");
    setMaxSubTurns("");
    setDeadlineMs("");
    setJobType("");
    setParentAgentType("");
    setParentAgentID("");
  };

  const submit = async () => {
    if (sending) return;
    setError(null);

    if (prompt.trim() === "") {
      setError("Prompt is required.");
      return;
    }
    const specs = repoSpecs.map((s) => s.trim()).filter((s) => s !== "");
    if (specs.length === 0) {
      setError("Name at least one repository to clone (URL#branch).");
      return;
    }
    if (permission === "") {
      setError("Choose a permission mode — there is no default.");
      return;
    }

    let schema: unknown;
    if (resultSchema.trim() !== "") {
      try {
        schema = JSON.parse(resultSchema);
      } catch {
        setError("result_schema must be valid JSON.");
        return;
      }
    }
    const numbers: { raw: string; name: string }[] = [
      { raw: maxSubTurns, name: "max_sub_turns" },
      { raw: deadlineMs, name: "deadline_ms" },
    ];
    for (const { raw, name } of numbers) {
      if (raw.trim() !== "") {
        const n = Number(raw);
        if (!Number.isFinite(n) || n < 0 || !Number.isInteger(n)) {
          setError(`${name} must be a non-negative integer.`);
          return;
        }
      }
    }

    const body: WorkRequest = {
      prompt: prompt.trim(),
      repos: specs.map(parseRepoSpec),
      permission_mode: permission,
    };
    if (model.trim() !== "") body.model = model.trim();
    if (effort !== "") body.effort = effort;
    const denyList = deny.split(",").map((d) => d.trim()).filter((d) => d !== "");
    if (denyList.length > 0) body.deny = denyList;
    if (schema !== undefined) body.result_schema = schema;
    if (maxSubTurns.trim() !== "") body.max_sub_turns = Number(maxSubTurns);
    if (deadlineMs.trim() !== "") body.deadline_ms = Number(deadlineMs);
    if (jobType !== "") body.job_type = jobType;
    if (parentAgentType.trim() !== "") body.parent_agent_type = parentAgentType.trim();
    if (parentAgentID.trim() !== "") body.parent_agent_id = parentAgentID.trim();

    setSending(true);
    try {
      const res = await startRun(token, body);
      setAccepted(res.request_id);
      reset();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSending(false);
    }
  };

  return (
    <div className="start-form-card">
      <div className="start-form">
        <div className="start-form-head">
          <span className="start-title">Start a run</span>
          <Button variant="outline" size="sm" onClick={onClose}>
            Close
          </Button>
        </div>

        <label className="start-field start-field-wide">
          <span className="start-label">Prompt</span>
          <textarea
            className="start-prompt"
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="The task for the agent to perform."
            rows={3}
            spellCheck={false}
          />
        </label>

        <div className="start-field start-field-wide">
          <span className="start-label">Repositories</span>
          {repoSpecs.map((spec, i) => (
            <div className="start-repo-row" key={i}>
              <Input
                className="start-input"
                value={spec}
                onChange={(e) => setRepoAt(i, e.target.value)}
                placeholder="https://github.com/org/app.git#branch"
                spellCheck={false}
                aria-label={`Repository ${i + 1} (URL#branch)`}
              />
              {repoSpecs.length > 1 && (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setRepoSpecs((prev) => prev.filter((_, j) => j !== i))}
                >
                  Remove
                </Button>
              )}
            </div>
          ))}
          <Button variant="outline" size="sm" onClick={() => setRepoSpecs((prev) => [...prev, ""])}>
            Add repository
          </Button>
        </div>

        <div className="start-field start-field-wide">
          <span className="start-label">Permission mode</span>
          <ToggleGroup type="single" value={permission} onValueChange={(v) => v && setPermission(v)}>
            <ToggleGroupItem value="readonly">readonly</ToggleGroupItem>
            <ToggleGroupItem value="full">full</ToggleGroupItem>
          </ToggleGroup>
          <p className="hint">
            An explicit choice — there is no default. readonly: read-only tools only. full: everything,
            as root, in the workspace — and the harness container has the host&rsquo;s docker socket, so a
            full run has the host daemon.
          </p>
        </div>

        <Collapsible className="start-optional">
          <CollapsibleTrigger asChild>
            <Button variant="outline" size="sm" className="start-optional-trigger">
              Optional fields
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            <div className="start-optional-grid">
              <label className="start-field">
                <span className="start-label">Model</span>
                <Input className="start-input" value={model} onChange={(e) => setModel(e.target.value)} placeholder="deepseek-v4-pro" spellCheck={false} />
              </label>
              <label className="start-field">
                <span className="start-label">Effort</span>
                <select className="start-select" value={effort} onChange={(e) => setEffort(e.target.value)}>
                  <option value="">(config default)</option>
                  <option value="low">low</option>
                  <option value="high">high</option>
                  <option value="max">max</option>
                </select>
              </label>
              <label className="start-field">
                <span className="start-label">Deny patterns</span>
                <Input className="start-input" value={deny} onChange={(e) => setDeny(e.target.value)} placeholder="comma-separated substrings, e.g. git push" spellCheck={false} />
              </label>
              <label className="start-field">
                <span className="start-label">Job type</span>
                <select className="start-select" value={jobType} onChange={(e) => setJobType(e.target.value)}>
                  <option value="">(implementation)</option>
                  <option value="implementation">implementation</option>
                  <option value="orchestration">orchestration</option>
                </select>
              </label>
              <label className="start-field">
                <span className="start-label">Max sub-turns</span>
                <Input className="start-input" type="number" min={0} value={maxSubTurns} onChange={(e) => setMaxSubTurns(e.target.value)} placeholder="(config default)" />
              </label>
              <label className="start-field">
                <span className="start-label">Deadline (ms)</span>
                <Input className="start-input" type="number" min={0} value={deadlineMs} onChange={(e) => setDeadlineMs(e.target.value)} placeholder="(config default)" />
              </label>
              <label className="start-field">
                <span className="start-label">Parent agent type</span>
                <Input className="start-input" value={parentAgentType} onChange={(e) => setParentAgentType(e.target.value)} placeholder="claude-code, cursor, or user" spellCheck={false} />
              </label>
              <label className="start-field">
                <span className="start-label">Parent agent id</span>
                <Input className="start-input" value={parentAgentID} onChange={(e) => setParentAgentID(e.target.value)} placeholder="(empty when type is user)" spellCheck={false} />
              </label>
              <label className="start-field start-field-wide">
                <span className="start-label">Result schema</span>
                <textarea
                  className="start-schema"
                  value={resultSchema}
                  onChange={(e) => setResultSchema(e.target.value)}
                  placeholder='{"type": "object", ...} — JSON Schema the Complete result must satisfy'
                  rows={3}
                  spellCheck={false}
                />
              </label>
            </div>
          </CollapsibleContent>
        </Collapsible>

        <div className="start-actions">
          <Button onClick={submit} disabled={sending}>
            {sending ? "Starting…" : "Start run"}
          </Button>
          {error && <span className="field-error">{error}</span>}
        </div>

        {accepted && (
          <div className="start-accepted">
            {started ? (
              <>
                Session <code>{started.id}</code> started — it is in the list above.
                <Button variant="outline" size="sm" onClick={() => onOpen(started.id)}>
                  Open transcript
                </Button>
              </>
            ) : (
              <>
                Accepted as <code>{accepted}</code>. Waiting for the pool to claim it — the session
                will appear in the list when it starts.
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
