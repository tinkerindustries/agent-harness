import { useEffect, useState, useSyncExternalStore } from "react";
import { errorMessage, parseRepoSpec, startRun, type WorkRequest } from "../api/operations";
import { filterRepos, listGithubRepos, repoSpecFor, type GithubRepo } from "../api/github";
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
// frontend"): repos (URL#branch, repeatable), a permission mode defaulting to
// full, and the optional fields behind a disclosure so the common case stays
// short. There is no prompt field: the run is created empty and the operator
// types the first message into the session once it appears, which is why
// submitting both POSTs the work request and — once the pool claims it and
// the session shows up on the list feed — opens that session's transcript
// (docs/RUN-CONTROL.md "Start"). The permission mode carries the one warning
// this surface must not bury: full runs as root in the workspace, and the
// harness container has the host's docker socket, so a full run has the host
// daemon.
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
  const [repoSpecs, setRepoSpecs] = useState<string[]>([""]);
  const [permission, setPermission] = useState("full");
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

  // The repo picker's async state, fetched once on mount: the operator's
  // GitHub repos for the searchable combobox, whether a github.token is
  // configured at all, and the fetch error when the GitHub call failed. All
  // three are non-blocking — a missing token or a failed fetch just
  // suppresses suggestions, never the form (docs/DATA-API.md "github repos").
  const [githubRepos, setGithubRepos] = useState<GithubRepo[]>([]);
  const [githubConfigured, setGithubConfigured] = useState(false);
  const [githubLoading, setGithubLoading] = useState(true);
  const [githubError, setGithubError] = useState<string | null>(null);
  // openRow is the repo row whose suggestion list is showing; activeIndex is
  // the keyboard cursor inside it (ArrowUp/ArrowDown move it, Enter picks).
  const [openRow, setOpenRow] = useState<number | null>(null);
  const [activeIndex, setActiveIndex] = useState(0);

  useEffect(() => {
    let cancelled = false;
    listGithubRepos()
      .then((res) => {
        if (cancelled) return;
        setGithubRepos(res.repos);
        setGithubConfigured(res.configured);
      })
      .catch((err) => {
        if (cancelled) return;
        setGithubError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setGithubLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // The screen follows the new session from the list feed the screen is
  // already connected to: a session whose request_id matches what the 202
  // named is the one this form started. No polling, no placeholder row —
  // the feed is what says a session started.
  const snapshot = useSyncExternalStore(sessionListStore.subscribe, sessionListStore.getSnapshot);
  const started = accepted ? snapshot.sessions.find((s) => s.request_id === accepted) : undefined;

  // Once the pool claims the request and the session appears on the feed,
  // take the operator straight to it: with no prompt field there is nothing
  // left to do on this form, and the session's own transcript is where the
  // first message gets typed. The 202 branch below covers the moments before
  // the feed sees the session.
  useEffect(() => {
    if (!started) return;
    onOpen(started.id);
    onClose();
  }, [started?.id, onOpen, onClose]);

  const setRepoAt = (i: number, v: string) => {
    setRepoSpecs((prev) => prev.map((spec, j) => (j === i ? v : spec)));
  };

  // matchesFor is row i's suggestions: the loaded repos whose full_name
  // contains the row's current text, case-insensitively. Empty while the row
  // is empty, while the picker is loading, or when the text matches nothing —
  // a URL the operator is typing manually matches nothing by construction, so
  // free-text entry keeps working exactly as before and the suggestions stay
  // purely additive.
  const matchesFor = (i: number): GithubRepo[] =>
    githubConfigured && !githubLoading && !githubError ? filterRepos(githubRepos, repoSpecs[i]) : [];

  // selectRepo fills row i with the suggestion's URL#branch spec — the exact
  // format parseRepoSpec already reads — and closes that row's list.
  const selectRepo = (i: number, repo: GithubRepo) => {
    setRepoAt(i, repoSpecFor(repo));
    setOpenRow(null);
  };

  // handleRepoKeyDown runs the picker's keyboard navigation on one repo row's
  // input: ArrowDown/ArrowUp move the cursor through the open list (opening
  // it on the first ArrowDown), Enter picks the highlighted suggestion, and
  // Escape closes the list. Any other key falls through to the input.
  const handleRepoKeyDown = (i: number, e: React.KeyboardEvent<HTMLInputElement>) => {
    const matches = matchesFor(i);
    if (e.key === "ArrowDown") {
      e.preventDefault();
      if (openRow !== i) {
        setOpenRow(i);
        setActiveIndex(0);
      } else if (matches.length > 0) {
        setActiveIndex((a) => Math.min(a + 1, matches.length - 1));
      }
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      if (openRow === i) {
        setActiveIndex((a) => Math.max(a - 1, 0));
      }
    } else if (e.key === "Enter") {
      if (openRow === i && matches.length > 0) {
        e.preventDefault();
        selectRepo(i, matches[Math.min(activeIndex, matches.length - 1)]);
      }
    } else if (e.key === "Escape") {
      setOpenRow(null);
    }
  };

  const reset = () => {
    setRepoSpecs([""]);
    setPermission("full");
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

    const specs = repoSpecs.map((s) => s.trim()).filter((s) => s !== "");
    if (specs.length === 0) {
      setError("Name at least one repository to clone (URL#branch).");
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

        <div className="start-field start-field-wide">
          <span className="start-label">Repositories</span>
          {repoSpecs.map((spec, i) => {
            const matches = matchesFor(i);
            return (
              <div className="start-repo-row" key={i}>
                <div className="start-repo-combobox">
                  <Input
                    className="start-input"
                    value={spec}
                    onChange={(e) => {
                      setRepoAt(i, e.target.value);
                      setActiveIndex(0);
                      setOpenRow(i);
                    }}
                    onFocus={() => {
                      setOpenRow(i);
                      setActiveIndex(0);
                    }}
                    onBlur={() => setOpenRow(null)}
                    onKeyDown={(e) => handleRepoKeyDown(i, e)}
                    placeholder="https://github.com/org/app.git#branch"
                    spellCheck={false}
                    aria-label={`Repository ${i + 1} (URL#branch)`}
                    aria-expanded={openRow === i && matches.length > 0}
                    aria-controls={openRow === i && matches.length > 0 ? `repo-suggestions-${i}` : undefined}
                  />
                  {openRow === i && matches.length > 0 && (
                    <div
                      id={`repo-suggestions-${i}`}
                      className="start-repo-suggestions"
                      role="listbox"
                      aria-label="Repository suggestions"
                    >
                      {matches.map((repo, j) => (
                        <button
                          key={repo.full_name}
                          type="button"
                          role="option"
                          aria-selected={j === activeIndex}
                          className={
                            j === activeIndex
                              ? "start-repo-suggestion start-repo-suggestion-active"
                              : "start-repo-suggestion"
                          }
                          // mousedown preventDefault keeps the input focused,
                          // so the input's onBlur cannot close the list
                          // before the click lands.
                          onMouseDown={(e) => e.preventDefault()}
                          onClick={() => selectRepo(i, repo)}
                        >
                          <span className="start-repo-suggestion-name">{repo.full_name}</span>
                          <span className="start-repo-suggestion-meta">
                            {repo.default_branch}
                            {repo.private ? " · private" : ""}
                          </span>
                        </button>
                      ))}
                    </div>
                  )}
                </div>
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
            );
          })}
          <Button variant="outline" size="sm" onClick={() => setRepoSpecs((prev) => [...prev, ""])}>
            Add repository
          </Button>
          {githubError && <p className="hint">{githubError}</p>}
          {!githubConfigured && !githubLoading && !githubError && (
            <p className="hint">Add a GitHub token in Settings to search your repositories.</p>
          )}
        </div>

        <div className="start-field start-field-wide">
          <span className="start-label">Permission mode</span>
          <ToggleGroup type="single" value={permission} onValueChange={(v) => v && setPermission(v)}>
            <ToggleGroupItem value="readonly">readonly</ToggleGroupItem>
            <ToggleGroupItem value="full">full</ToggleGroupItem>
          </ToggleGroup>
          <p className="hint">
            Defaults to full. full: everything, as root, in the workspace — and the harness container has
            the host&rsquo;s docker socket, so a full run has the host daemon. readonly: read-only tools only.
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
