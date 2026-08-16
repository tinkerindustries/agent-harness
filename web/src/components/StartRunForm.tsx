import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { errorMessage, parseRepoSpec, startRun, type WorkRequest } from "../api/operations";
import { filterRepos, listGithubRepos, repoSpecFor, type GithubRepo } from "../api/github";
import { DEFAULT_MODEL_KEY, listModels, resolveModelOptions, settingModel } from "../api/models";
import { sessionListStore } from "../api/sessionListStore";
import { listSettings } from "../api/settings";
import {
  attachmentCapsFromSettings,
  formatFileSize,
  readAttachmentFiles,
  type AttachmentCaps,
  type ChosenAttachment,
} from "../api/attachments";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "./ui/collapsible";
import { ToggleGroup, ToggleGroupItem } from "./ui/toggle-group";
import { cn } from "@/lib/utils";

// StartRunForm is the browser's start form (docs/RUN-CONTROL.md "The
// frontend"): repos (URL#branch, repeatable), a model, a thinking effort, a
// permission mode defaulting to full, and the optional fields behind a
// disclosure. The permission mode carries the one warning this surface must
// not bury: full runs as root in the workspace, and the harness container has
// the host's docker socket, so a full run has the host daemon.
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
  // The model dropdown is fed by the harness, not a hardcoded copy: the
  // option list comes from GET /api/models — the provider table that
  // validates a work request — and the preselected model and the reset
  // target come from the model.default settings row, so a model added to the
  // table shows up in the browser with no frontend change (docs/DATA-API.md
  // "models"). Empty until the mount fetch resolves; the select renders no
  // selection rather than a guessed one. When the models request fails,
  // resolveModelOptions falls back to the settings' two model defaults so
  // the dropdown is never empty.
  const [model, setModel] = useState("");
  const [models, setModels] = useState<string[]>([]);
  const [defaultModel, setDefaultModel] = useState("");
  const [modelsError, setModelsError] = useState<string | null>(null);
  const [effort, setEffort] = useState("max");
  const [deny, setDeny] = useState("");
  const [resultSchema, setResultSchema] = useState("");
  const [maxSubTurns, setMaxSubTurns] = useState("");
  const [deadlineMs, setDeadlineMs] = useState("");
  const [jobType, setJobType] = useState("");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [parentAgentType, setParentAgentType] = useState("");
  const [parentAgentID, setParentAgentID] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [accepted, setAccepted] = useState<string | null>(null);

  // The attachment input's async state: the caps POST /api/runs enforces,
  // read from GET /api/settings once on mount so an operator who changes
  // tools.attachments_max_count or tools.attachments_max_bytes sees the form
  // change with it. The input stays disabled until the caps land — there is
  // no hardcoded fallback to validate against. chosen are the files accepted
  // so far, shown with a per-file remove control until the run is submitted.
  const [attachmentCaps, setAttachmentCaps] = useState<AttachmentCaps | null>(null);
  const [attachmentCapsError, setAttachmentCapsError] = useState<string | null>(null);
  const [chosen, setChosen] = useState<ChosenAttachment[]>([]);

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
  // firstRepoInputRef targets the first repo row's input so mount can focus
  // it and open its suggestion list.
  const firstRepoInputRef = useRef<HTMLInputElement>(null);

  // Mount is the hook that puts the cursor in the first repo row with its
  // suggestion list already open; the list renders once the repo fetch lands.
  useEffect(() => {
    firstRepoInputRef.current?.focus();
    setOpenRow(0);
    setActiveIndex(0);
  }, []);

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

  // Mount resolves the form's two server-fed bits in one pass. The
  // attachment caps and the model defaults come from the settings registry,
  // and the model dropdown's options come from GET /api/models — the
  // provider table that validates a work request, so a model added there
  // shows up here with no frontend change (docs/DATA-API.md "models"). The
  // two fetches are independent, so one failing must not take the other
  // down: a failed settings fetch disables the attachment input with the
  // reason shown, and a failed models fetch falls back to the settings'
  // model defaults (resolveModelOptions) with a hint — never an empty
  // dropdown.
  useEffect(() => {
    let cancelled = false;
    Promise.allSettled([listModels(), listSettings()]).then(([modelsRes, settingsRes]) => {
      if (cancelled) return;
      if (settingsRes.status === "fulfilled") {
        const entries = settingsRes.value;
        setAttachmentCaps(attachmentCapsFromSettings(entries));
        const def = settingModel(entries, DEFAULT_MODEL_KEY);
        setDefaultModel(def);
        // The user cannot have picked anything yet — the select has no
        // options until this lands — so filling an empty selection is safe.
        setModel((prev) => (prev === "" ? def : prev));
      } else {
        setAttachmentCapsError(`Attachment limits unavailable: ${errorMessage(settingsRes.reason)}`);
      }
      if (modelsRes.status === "rejected") {
        setModelsError(`Model list unavailable: ${errorMessage(modelsRes.reason)} — showing the configured defaults.`);
      }
      setModels(
        resolveModelOptions(
          modelsRes.status === "fulfilled" ? modelsRes.value : null,
          settingsRes.status === "fulfilled" ? settingsRes.value : [],
        ),
      );
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // handleAttachmentChange reads a selection through readAttachmentFiles —
  // extension and caps enforced there, with the refusal naming the actual
  // limit — and appends the accepted files to the chosen list. The input's
  // value is cleared so picking the same file again re-fires the change.
  const handleAttachmentChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(e.target.files ?? []);
    e.target.value = "";
    if (files.length === 0 || !attachmentCaps) return;
    setError(null);
    const result = await readAttachmentFiles(files, attachmentCaps);
    if (!result.ok) {
      setError(result.error);
      return;
    }
    setChosen((prev) => [...prev, ...result.chosen]);
  };

  const removeAttachment = (name: string) => {
    setChosen((prev) => prev.filter((c) => c.attachment.name !== name));
  };

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

  // matchesFor is row i's suggestions: every loaded repo while the row is
  // empty (focusing the row opens the picker), narrowed to those whose
  // full_name contains the row's text, case-insensitively. Empty while the
  // picker is loading or when the text matches nothing — a URL the operator
  // is typing manually matches nothing by construction, so free-text entry
  // keeps working exactly as before and the suggestions stay purely additive.
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
    // The reset target is the settings-derived default (model.default), the
    // same source the initial selection came from — never a hardcoded name.
    setModel(defaultModel);
    setEffort("max");
    setDeny("");
    setResultSchema("");
    setMaxSubTurns("");
    setDeadlineMs("");
    setJobType("");
    setTitle("");
    setDescription("");
    setParentAgentType("");
    setParentAgentID("");
    setChosen([]);
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
      model: model.trim(),
      effort,
    };
    const denyList = deny.split(",").map((d) => d.trim()).filter((d) => d !== "");
    if (denyList.length > 0) body.deny = denyList;
    if (schema !== undefined) body.result_schema = schema;
    if (maxSubTurns.trim() !== "") body.max_sub_turns = Number(maxSubTurns);
    if (deadlineMs.trim() !== "") body.deadline_ms = Number(deadlineMs);
    if (jobType !== "") body.job_type = jobType;
    // Title and description are optional on the browser path: a run started
    // with both blank renders the raw prompt as its description line, the
    // way a pre-migration row does.
    if (title.trim() !== "") body.title = title.trim();
    if (description.trim() !== "") body.description = description.trim();
    if (chosen.length > 0) body.attachments = chosen.map((c) => c.attachment);

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
    <div className="mb-4 rounded-lg border border-border bg-card">
      <div className="flex flex-col gap-3 px-4 py-3.5">
        <div className="flex items-center gap-2">
          <span className="flex-1 font-semibold">Start a run</span>
          <Button variant="outline" size="sm" onClick={onClose}>
            Close
          </Button>
        </div>

        <div className="flex flex-col gap-1">
          <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Repositories</span>
          {repoSpecs.map((spec, i) => {
            const matches = matchesFor(i);
            return (
              <div className="flex items-center gap-2" key={i}>
                <div className="relative min-w-0 flex-1">
                  <Input
                    ref={i === 0 ? firstRepoInputRef : undefined}
                    className="font-mono text-sm"
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
                      className="absolute top-[calc(100%+4px)] right-0 left-0 z-20 flex max-h-[256px] flex-col gap-0.5 overflow-y-auto rounded-[calc(var(--radius)-2px)] border border-border bg-card p-1 [box-shadow:0_4px_12px_rgb(0_0_0/0.15)]"
                      role="listbox"
                      aria-label="Repository suggestions"
                    >
                      {matches.map((repo, j) => (
                        <button
                          key={repo.full_name}
                          type="button"
                          role="option"
                          aria-selected={j === activeIndex}
                          className={cn(
                            "flex h-12 w-full cursor-pointer flex-col justify-center gap-px rounded-[calc(var(--radius)-4px)] border-0 bg-transparent px-2 py-[5px] text-left text-foreground hover:bg-muted",
                            j === activeIndex && "bg-muted",
                          )}
                          // mousedown preventDefault keeps the input focused,
                          // so the input's onBlur cannot close the list
                          // before the click lands.
                          onMouseDown={(e) => e.preventDefault()}
                          onClick={() => selectRepo(i, repo)}
                        >
                          <span className="truncate font-mono text-sm">{repo.full_name}</span>
                          <span className="truncate text-micro text-muted-foreground">
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
            Add another repository
          </Button>
          {githubError && <p className="hint">{githubError}</p>}
          {!githubConfigured && !githubLoading && !githubError && (
            <p className="hint">Add a GitHub token in Settings to search your repositories.</p>
          )}
        </div>

        <div className="flex flex-col gap-1">
          <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Image attachments</span>
          <input
            type="file"
            accept=".png,.jpg,.jpeg,.webp"
            multiple
            disabled={!attachmentCaps}
            onChange={handleAttachmentChange}
            aria-label="Image attachments (PNG, JPEG, WebP) — the mockups the run works against"
          />
          {attachmentCapsError && <p className="hint">{attachmentCapsError}</p>}
          {!attachmentCaps && !attachmentCapsError && (
            <p className="hint">Loading attachment limits…</p>
          )}
          {attachmentCaps && (
            <p className="hint">
              Mockups the agent reviews against, e.g. the page it should match. PNG, JPEG or WebP, up to{" "}
              {attachmentCaps.maxCount} files of {formatFileSize(attachmentCaps.maxBytes)} each.
            </p>
          )}
          {chosen.length > 0 && (
            <ul className="m-0 flex list-none flex-col gap-1.5 p-0">
              {chosen.map(({ attachment, size }) => (
                <li className="flex items-center gap-2" key={attachment.name}>
                  <span className="min-w-0 flex-1 truncate font-mono text-sm">{attachment.name}</span>
                  <span className="text-xs whitespace-nowrap text-muted-foreground">{formatFileSize(size)}</span>
                  <Button variant="outline" size="sm" onClick={() => removeAttachment(attachment.name)}>
                    Remove
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>

        <div className="flex flex-wrap items-end gap-3.5 max-touch:flex-col max-touch:items-stretch">
          <label className="flex flex-col gap-1">
            <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Model</span>
            <select
              className="h-9 rounded-[calc(var(--radius)-2px)] border border-input bg-card px-2 font-[inherit] text-sm text-foreground outline-none max-phone:h-11"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            >
              {models.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
            {modelsError && <p className="hint">{modelsError}</p>}
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Thinking</span>
            <select
              className="h-9 rounded-[calc(var(--radius)-2px)] border border-input bg-card px-2 font-[inherit] text-sm text-foreground outline-none max-phone:h-11"
              value={effort}
              onChange={(e) => setEffort(e.target.value)}
            >
              <option value="max">max</option>
              <option value="high">high</option>
              <option value="low">low</option>
            </select>
          </label>
          <div className="flex flex-col gap-1">
            <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Permission mode</span>
            <ToggleGroup type="single" value={permission} onValueChange={(v) => v && setPermission(v)}>
              <ToggleGroupItem value="readonly">readonly</ToggleGroupItem>
              <ToggleGroupItem value="full">full</ToggleGroupItem>
            </ToggleGroup>
            <p className="hint">
              Defaults to full. full: everything, as root, in the workspace — and the harness container has
              the host&rsquo;s docker socket, so a full run has the host daemon. readonly: read-only tools only.
            </p>
          </div>
        </div>

        <Collapsible>
          <CollapsibleTrigger asChild>
            <Button variant="outline" size="sm">
              Optional fields
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            <div className="grid grid-cols-2 gap-x-3.5 gap-y-2.5 pt-2.5 max-touch:grid-cols-1">
              <label className="flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Title</span>
                <Input className="font-mono text-sm" value={title} onChange={(e) => setTitle(e.target.value)} placeholder="(optional, up to 10 words)" spellCheck={false} />
              </label>
              <label className="col-span-full flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Description</span>
                <textarea
                  className="w-full min-w-0 resize-y rounded-[calc(var(--radius)-2px)] border border-input bg-transparent px-2.5 py-1.5 font-[inherit] text-base leading-normal outline-none focus:border-ring focus:[box-shadow:0_0_0_3px_color-mix(in_srgb,var(--ring)_50%,transparent)]"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  placeholder="(optional, up to 50 words) — what change this run is making; shown under the title on the main page"
                  rows={2}
                  spellCheck={false}
                />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Deny patterns</span>
                <Input className="font-mono text-sm" value={deny} onChange={(e) => setDeny(e.target.value)} placeholder="comma-separated substrings, e.g. git push" spellCheck={false} />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Job type</span>
                <select
                  className="h-9 rounded-[calc(var(--radius)-2px)] border border-input bg-card px-2 font-[inherit] text-sm text-foreground outline-none max-phone:h-11"
                  value={jobType}
                  onChange={(e) => setJobType(e.target.value)}
                >
                  <option value="">(implementation)</option>
                  <option value="implementation">implementation</option>
                  <option value="orchestration">orchestration</option>
                </select>
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Max sub-turns</span>
                <Input className="font-mono text-sm" type="number" min={0} value={maxSubTurns} onChange={(e) => setMaxSubTurns(e.target.value)} placeholder="(config default)" />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Deadline (ms)</span>
                <Input className="font-mono text-sm" type="number" min={0} value={deadlineMs} onChange={(e) => setDeadlineMs(e.target.value)} placeholder="(config default)" />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Parent agent type</span>
                <Input className="font-mono text-sm" value={parentAgentType} onChange={(e) => setParentAgentType(e.target.value)} placeholder="claude-code, cursor, or user" spellCheck={false} />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Parent agent id</span>
                <Input className="font-mono text-sm" value={parentAgentID} onChange={(e) => setParentAgentID(e.target.value)} placeholder="(empty when type is user)" spellCheck={false} />
              </label>
              <label className="col-span-full flex flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Result schema</span>
                <textarea
                  className="w-full min-w-0 resize-y rounded-[calc(var(--radius)-2px)] border border-input bg-transparent px-2.5 py-1.5 font-[inherit] text-base leading-normal outline-none focus:border-ring focus:[box-shadow:0_0_0_3px_color-mix(in_srgb,var(--ring)_50%,transparent)]"
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

        <div className="flex flex-wrap items-center gap-3">
          <Button onClick={submit} disabled={sending}>
            {sending ? "Starting…" : "Start run"}
          </Button>
          {error && <span className="field-error">{error}</span>}
        </div>

        {accepted && (
          <div className="flex flex-wrap items-center gap-2 rounded-[calc(var(--radius)-2px)] border border-border border-l-[3px] border-l-[var(--status-done)] bg-[var(--status-done-bg)] px-3 py-2 text-sm">
            {started ? (
              <>
                Session <code className="rounded bg-muted px-1 font-mono">{started.id}</code> started — it is in the
                list above.
                <Button variant="outline" size="sm" onClick={() => onOpen(started.id)}>
                  Open transcript
                </Button>
              </>
            ) : (
              <>
                Accepted as <code className="rounded bg-muted px-1 font-mono">{accepted}</code>. Waiting for the pool
                to claim it — the session will appear in the list when it starts.
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
