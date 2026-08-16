import { useCallback, useEffect, useMemo, useState } from "react";
import { CaretRight, MagnifyingGlass } from "@phosphor-icons/react";
import { deleteSetting, listSettings, secretMask, setSetting } from "../api/settings";
import type { SettingEntry } from "../api/settings";
import { cn } from "@/lib/utils";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "./ui/collapsible";
import { Input } from "./ui/input";
import { ToggleGroup, ToggleGroupItem } from "./ui/toggle-group";
import { useNavRight } from "./TopNav";

// RowState is one setting row's local UI state. draft is the value being
// typed; it is never seeded from the server's display value, because for a
// secret that value is a mask and writing the mask back would store it as
// the key. error carries the server's message from the last failed write
// (docs/DESIGN.md §4.2: a 400, 415, or 403 all carry {"error": "..."}) so a
// failed save shows up next to the field instead of looking like a success.
interface RowState {
  draft: string;
  busy: boolean;
  error: string | null;
}

// groupBy renders the server's registry order into heading groups, keeping
// first-seen order so the screen shows the same grouping the CLI prints.
function groupBy(entries: SettingEntry[]): [string, SettingEntry[]][] {
  const groups: [string, SettingEntry[]][] = [];
  const byGroup = new Map<string, SettingEntry[]>();
  for (const entry of entries) {
    let list = byGroup.get(entry.group);
    if (!list) {
      list = [];
      byGroup.set(entry.group, list);
      groups.push([entry.group, list]);
    }
    list.push(entry);
  }
  return groups;
}

// inputType picks the input kind for a registry type: integers get a number
// input, secrets a password field (input masking only — the display value
// beside the field is whatever mask the server decided), everything else
// text. Durations stay text: "30s", "10m", "1h" — a plain number input would
// invite the wrong unit.
function inputType(entry: SettingEntry): "number" | "password" | "text" {
  if (entry.secret) return "password";
  if (entry.type === "integer") return "number";
  return "text";
}

// rowBadge is the badge vocabulary: only the exceptions are badged. A
// secret that is set says SET (its default is the
// empty string, so "override" would be technically true and say nothing), a
// missing credential says NOT SET, a stored value that differs from its
// default says OVERRIDE, and a row sitting at its default carries no badge
// at all — the value column already carries the state by weight.
function rowBadge(
  entry: SettingEntry,
): { label: string; variant: "running" | "gaveup" } | null {
  if (entry.secret) {
    return entry.set
      ? { label: "SET", variant: "running" }
      : { label: "NOT SET", variant: "gaveup" };
  }
  return entry.override ? { label: "OVERRIDE", variant: "running" } : null;
}

// displayValue is the closed row's value column: a set key shows the server's
// value (the mask for a secret), an unset secret says "not set" rather than
// rendering with a hole, and an unset ordinary key shows the default it
// resolves to. A set secret's mask is re-rendered at a fixed width
// (secretMask), so the row never reports how long the stored secret is.
function displayValue(entry: SettingEntry): string {
  if (entry.secret && !entry.set) return "not set";
  if (entry.secret) return secretMask(entry.value ?? "");
  return entry.set ? (entry.value ?? "") : entry.default;
}

// valueClass carries the state by weight: a stored value at full weight,
// a missing credential in the gave-up colour, and a value that is only
// the registry default muted.
function valueClass(entry: SettingEntry): string | undefined {
  if (entry.secret && !entry.set) return "text-[var(--status-gaveup)]";
  return entry.set ? "font-semibold text-foreground" : undefined;
}

// typeLabel names the registry type the way the mock's facts row does,
// folding the flags that change how the row is rendered: "string, secret",
// "string, closed set".
function typeLabel(entry: SettingEntry): string {
  let label = entry.type;
  if (entry.secret) label += ", secret";
  if (entry.allowed && entry.allowed.length > 0) label += ", closed set";
  return label;
}

// UnsetNotice is the sentence that says what stops working when a credential
// is missing. A secret's registry default is the empty string, so the old
// "not set — default  applies" rendered with a hole in it and understated
// the case; the true sentence is per-credential. The two credentials
// today have one consumer each: the harness itself, and the three vision
// tools that call Gemini (Glance, Ground, Detect — Crop makes no model call
// and needs no key).
interface UnsetNotice {
  lead: string;
  rest: string;
}

function unsetSecretNotice(entry: SettingEntry): UnsetNotice | null {
  if (!entry.secret || entry.set) return null;
  switch (entry.key) {
    case "deepseek.api_key":
      return {
        lead: "The harness's own account.",
        rest: " No run can talk to DeepSeek until this is set.",
      };
    case "google.api_key":
      return {
        lead: "Glance, Ground and Detect fail until this is set.",
        rest: " Crop makes no model call and needs no key. Nothing else in the harness reads it — runs, tools and the queue are unaffected.",
      };
    default:
      return { lead: "Nothing that reads this credential works until it is set.", rest: "" };
  }
}

// Filter is which rows the list shows: everything, only the stored values
// that differ from their default (the four overrides), or only the missing
// credentials.
type Filter = "all" | "override" | "attention";

// SettingRow is one registry entry. Closed, it is a single grid line — caret,
// key, value, description, badges — and the only thing in the tab order on a
// freshly opened screen. Open, it carries the full description, the bounds,
// and the write controls. A setting with a closed set of allowed values
// (model.effort) renders a ToggleGroup that saves on selection instead of a
// text input: the registry knows the accepted values, so the screen should
// not make an operator discover them through a 400.
interface RowProps {
  entry: SettingEntry;
  row: RowState;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onDraft: (key: string, draft: string) => void;
  onSave: (key: string) => void;
  onSaveAllowed: (key: string, value: string) => void;
  onClear: (key: string) => void;
}

function SettingRow({
  entry,
  row,
  open,
  onOpenChange,
  onDraft,
  onSave,
  onSaveAllowed,
  onClear,
}: RowProps) {
  const badge = rowBadge(entry);
  const notice = unsetSecretNotice(entry);
  const placeholder = entry.secret
    ? entry.set
      ? "replace current value"
      : "paste the key"
    : entry.default;
  // The current value a ToggleGroup starts pressed on: the stored value, or
  // the default for a key that has nothing stored. Only Go can make the
  // pressed state wrong, and only by storing a value the registry accepted.
  const current = entry.set ? (entry.value ?? "") : entry.default;

  return (
    <Collapsible
      className="bg-card [&+&]:border-t [&+&]:border-border"
      open={open}
      onOpenChange={onOpenChange}
    >
      <CollapsibleTrigger asChild>
        <button
          type="button"
          className={cn(
            "grid w-full cursor-pointer grid-cols-[10px_minmax(150px,250px)_minmax(80px,160px)_minmax(0,1fr)_auto] items-center gap-2.5 border-0 bg-transparent px-3 py-2 text-left font-[inherit] text-inherit hover:bg-[var(--surface-hover)] max-tablet:grid-cols-[10px_minmax(0,1fr)_auto]",
            open && "bg-[color-mix(in_srgb,var(--accent)_40%,transparent)]",
          )}
        >
          <CaretRight
            className={cn(
              "text-muted-foreground [transition:transform_var(--dur-caret)_var(--ease)]",
              open && "rotate-90",
            )}
          />
          <span className="font-mono text-sm">{entry.key}</span>
          <span className={cn("truncate text-right font-mono text-sm text-muted-foreground tabular-nums max-tablet:col-start-2 max-tablet:text-left", valueClass(entry))}>
            {displayValue(entry)}
          </span>
          {/* The truncated description stays in the closed header line only;
              the expanded body repeats it in full below, so an open row must
              not show it twice. */}
          {!open && (
            <span className="min-w-0 truncate text-sm text-muted-foreground max-tablet:hidden">{entry.description}</span>
          )}
          <span className="flex justify-end gap-1.5">
            {badge && <Badge variant={badge.variant}>{badge.label}</Badge>}
          </span>
        </button>
      </CollapsibleTrigger>
      <CollapsibleContent>
        {/* The body mounts when the row opens, so .anim-stream-in fires on the
            open and never on load — the same "it just arrived" gesture the
            transcript's committed prose gets. Height is not animated: the row
            below moves once, when the content appears. */}
        <div className="anim-stream-in grid max-w-[92ch] gap-2.5 pt-0.5 pr-3 pb-3.5 pl-8">
          <p className="m-0 max-w-[78ch] text-sm">{entry.description}</p>
          {notice && (
            <div className="rounded-[calc(var(--radius)-2px)] border border-border border-l-[3px] border-l-[var(--status-gaveup)] bg-[var(--status-gaveup-bg)] px-3 py-2 text-sm text-foreground">
              <b>{notice.lead}</b>
              {notice.rest}
            </div>
          )}
          <div className="facts">
            <span>
              <span className="mr-[5px] text-micro tracking-[0.06em] uppercase">Type</span> {typeLabel(entry)}
            </span>
            {entry.set && (
              <span>
                <span className="mr-[5px] text-micro tracking-[0.06em] uppercase">Stored</span>{" "}
                <code className="rounded bg-muted px-[5px] py-px text-foreground">
                  {entry.secret ? secretMask(entry.value ?? "") : entry.value}
                </code>
              </span>
            )}
            {(!entry.secret || !entry.set) && (
              <span>
                <span className="mr-[5px] text-micro tracking-[0.06em] uppercase">Default</span>{" "}
                <code className="rounded bg-muted px-[5px] py-px text-foreground">
                  {entry.default === "" ? "none" : entry.default}
                </code>
              </span>
            )}
            {entry.min !== undefined && entry.max !== undefined && (
              <span>
                <span className="mr-[5px] text-micro tracking-[0.06em] uppercase">Range</span>{" "}
                <code className="rounded bg-muted px-[5px] py-px text-foreground">{entry.min}</code> –{" "}
                <code className="rounded bg-muted px-[5px] py-px text-foreground">{entry.max}</code>
              </span>
            )}
            {entry.allowed && entry.allowed.length > 0 && (
              <span>
                <span className="mr-[5px] text-micro tracking-[0.06em] uppercase">Allowed</span>{" "}
                {entry.allowed.map((v) => (
                  <code key={v} className="rounded bg-muted px-[5px] py-px text-foreground">
                    {v}
                  </code>
                ))}
              </span>
            )}
            <span>
              <span className="mr-[5px] text-micro tracking-[0.06em] uppercase">Takes effect</span>{" "}
              {entry.restart ? "next start" : "next run"}
            </span>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            {entry.allowed && entry.allowed.length > 0 ? (
              <>
                <ToggleGroup
                  type="single"
                  variant="outline"
                  size="sm"
                  value={current}
                  onValueChange={(v) => {
                    if (v && v !== current) onSaveAllowed(entry.key, v);
                  }}
                  disabled={row.busy}
                >
                  {entry.allowed.map((v) => (
                    <ToggleGroupItem key={v} value={v}>
                      {v}
                    </ToggleGroupItem>
                  ))}
                </ToggleGroup>
                <span className="hint">
                  Saves on selection. The default is the pressed one until it differs.
                </span>
              </>
            ) : (
              <>
                <Input
                  className="min-w-[300px] flex-[0_1_320px] font-mono max-phone:min-w-0 max-phone:basis-full"
                  type={inputType(entry)}
                  placeholder={placeholder}
                  value={row.draft}
                  onChange={(ev) => onDraft(entry.key, ev.target.value)}
                  disabled={row.busy}
                  spellCheck={false}
                  autoComplete="off"
                />
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => onSave(entry.key)}
                  disabled={row.busy || row.draft === ""}
                >
                  Save
                </Button>
                {entry.set && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => onClear(entry.key)}
                    disabled={row.busy}
                    title="delete the stored value so the registry default applies"
                  >
                    {entry.secret ? "Clear" : `Reset to ${entry.default}`}
                  </Button>
                )}
              </>
            )}
            {entry.type === "duration" && (
              <span className="hint">
                Go duration text: <code>30s</code>, <code>10m</code>, <code>1h</code>.
              </span>
            )}
            {entry.secret && (
              <span className="hint">
                The full value never leaves the process — the API masks it to its last four
                characters and has no reveal parameter. Reading one back is{" "}
                <code>harness config get -reveal</code>, at a terminal.
              </span>
            )}
            {row.error && <span className="field-error">{row.error}</span>}
          </div>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

export function SettingsScreen() {
  const [entries, setEntries] = useState<SettingEntry[] | null>(null);
  const [rows, setRows] = useState<Record<string, RowState>>({});
  // openKeys starts empty: the screen opens with every row closed, so nothing
  // is in the tab order but the disclosures and the back button.
  const [openKeys, setOpenKeys] = useState<Set<string>>(new Set());
  const [filter, setFilter] = useState<Filter>("all");
  const [loadError, setLoadError] = useState<string | null>(null);
  // query is the nav's search input: a client-side filter over the list
  // the screen already holds, key or description, combined with the
  // chip filter below.
  const [query, setQuery] = useState("");

  // refresh re-fetches the whole list and rebuilds the row bookkeeping on
  // the server's answer. After a successful write the screen shows whatever
  // the server decides to display — the masking lives there, and the client
  // never guesses at it. A surviving row keeps its draft, so typing in one
  // row is not wiped by another row's save triggering a refresh.
  const refresh = useCallback(async () => {
    try {
      const next = await listSettings();
      setEntries(next);
      setRows((prev) => {
        const merged: Record<string, RowState> = {};
        for (const entry of next) {
          merged[entry.key] = prev[entry.key] ?? { draft: "", busy: false, error: null };
        }
        return merged;
      });
      setLoadError(null);
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const counts = useMemo(() => {
    if (!entries) return null;
    return {
      total: entries.length,
      overridden: entries.filter((e) => e.override).length,
      restart: entries.filter((e) => e.restart).length,
      notSet: entries.filter((e) => e.secret && !e.set).length,
    };
  }, [entries]);

  const visibleEntries = useMemo(() => {
    if (!entries) return [];
    const base =
      filter === "all" ? entries : entries.filter((e) =>
        filter === "override" ? e.override : e.secret && !e.set,
      );
    const q = query.trim().toLowerCase();
    if (q === "") return base;
    return base.filter(
      (e) => e.key.toLowerCase().includes(q) || e.description.toLowerCase().includes(q),
    );
  }, [entries, filter, query]);

  const allOpen =
    visibleEntries.length > 0 && visibleEntries.every((e) => openKeys.has(e.key));

  function toggleOpen(key: string, open: boolean) {
    setOpenKeys((prev) => {
      const next = new Set(prev);
      if (open) next.add(key);
      else next.delete(key);
      return next;
    });
  }

  function toggleAll() {
    setOpenKeys(allOpen ? new Set() : new Set(visibleEntries.map((e) => e.key)));
  }

  function updateDraft(key: string, draft: string) {
    setRows((prev) => ({ ...prev, [key]: { ...prev[key], draft } }));
  }

  async function save(key: string) {
    const draft = rows[key].draft;
    if (draft === "") return; // nothing typed, nothing to write
    setRows((prev) => ({ ...prev, [key]: { ...prev[key], busy: true, error: null } }));
    try {
      await setSetting(key, draft);
      // Write landed; re-fetch so the displayed value is the server's, not
      // a client-side guess, then clear the draft.
      await refresh();
      setRows((prev) => ({ ...prev, [key]: { ...prev[key], draft: "", busy: false } }));
    } catch (err) {
      setRows((prev) => ({
        ...prev,
        [key]: {
          ...prev[key],
          busy: false,
          error: err instanceof Error ? err.message : String(err),
        },
      }));
    }
  }

  // saveAllowed is the ToggleGroup path: a closed set saves on selection —
  // the group only offers values the registry accepts, so there is nothing to
  // type and nothing to validate client-side (validation stays in Go; a
  // rejected write still surfaces the server's message under the row).
  async function saveAllowed(key: string, value: string) {
    if (rows[key]?.busy) return;
    setRows((prev) => ({ ...prev, [key]: { ...prev[key], busy: true, error: null } }));
    try {
      await setSetting(key, value);
      await refresh();
      setRows((prev) => ({ ...prev, [key]: { ...prev[key], busy: false } }));
    } catch (err) {
      setRows((prev) => ({
        ...prev,
        [key]: {
          ...prev[key],
          busy: false,
          error: err instanceof Error ? err.message : String(err),
        },
      }));
    }
  }

  async function clear(key: string) {
    setRows((prev) => ({ ...prev, [key]: { ...prev[key], busy: true, error: null } }));
    try {
      await deleteSetting(key);
      await refresh();
      setRows((prev) => ({ ...prev, [key]: { ...prev[key], busy: false } }));
    } catch (err) {
      setRows((prev) => ({
        ...prev,
        [key]: {
          ...prev[key],
          busy: false,
          error: err instanceof Error ? err.message : String(err),
        },
      }));
    }
  }

  // The nav's right slot for this screen: the key/description search
  // input.
  useNavRight(
    <Input
      type="search"
      icon={<MagnifyingGlass />}
      className="nav-search"
      placeholder="Filter by key or description…"
      value={query}
      onChange={(ev) => setQuery(ev.target.value)}
      spellCheck={false}
    />,
  );

  return (
    <div className="screen">
      {loadError && (
        <div className="rounded-[calc(var(--radius)-4px)] border border-[var(--status-failed)] p-2 mb-2 text-[0.85rem] text-[var(--status-failed)]">
          could not load settings: {loadError}
        </div>
      )}
      {entries === null && !loadError && <p className="dim">Loading settings…</p>}
      {entries !== null && counts && (
        <>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-[calc(var(--radius)-2px)] border border-border px-3 py-[7px] text-sm text-muted-foreground">
            <span>
              <b className="font-semibold text-foreground tabular-nums">{counts.total}</b> setting{counts.total === 1 ? "" : "s"}
            </span>
            <span className="text-border">·</span>
            <span>
              <b className="font-semibold text-foreground tabular-nums">{counts.overridden}</b> overridden
            </span>
            <span className="text-border">·</span>
            <span>
              <b className="font-semibold text-foreground tabular-nums">{counts.restart}</b> take
              {counts.restart === 1 ? "s" : ""} effect on the next start
            </span>
            <span className="text-border">·</span>
            {/* A zero count is good news: it renders neutral like the counts
                beside it, and only a non-zero count carries the amber. */}
            <span className={counts.notSet > 0 ? "text-[var(--status-gaveup)]" : undefined}>
              <b className="font-semibold text-foreground tabular-nums">{counts.notSet}</b> credential
              {counts.notSet === 1 ? "" : "s"} not set
            </span>
            <span className="flex-1" />
            <span>rows without a badge are at their default</span>
          </div>

          <div className="mt-3 mb-2 flex flex-wrap items-center gap-2">
            <button
              type="button"
              className={cn(
                "inline-flex h-[26px] items-center gap-[5px] rounded-full border border-border bg-background px-[9px] text-xs text-muted-foreground hover:bg-muted max-phone:min-h-11 max-phone:px-3.5",
                filter === "all" && "border-ring bg-secondary text-foreground",
              )}
              aria-pressed={filter === "all"}
              onClick={() => setFilter("all")}
            >
              All {counts.total}
            </button>
            <button
              type="button"
              className={cn(
                "inline-flex h-[26px] items-center gap-[5px] rounded-full border border-border bg-background px-[9px] text-xs text-muted-foreground hover:bg-muted max-phone:min-h-11 max-phone:px-3.5",
                filter === "override" && "border-ring bg-secondary text-foreground",
              )}
              aria-pressed={filter === "override"}
              onClick={() => setFilter("override")}
            >
              Overridden {counts.overridden}
            </button>
            <button
              type="button"
              className={cn(
                "inline-flex h-[26px] items-center gap-[5px] rounded-full border border-border bg-background px-[9px] text-xs text-muted-foreground hover:bg-muted max-phone:min-h-11 max-phone:px-3.5",
                filter === "attention" && "border-ring bg-secondary text-foreground",
              )}
              aria-pressed={filter === "attention"}
              onClick={() => setFilter("attention")}
            >
              Needs attention {counts.notSet}
            </button>
            <span className="flex-1" />
            <Button variant="outline" size="sm" onClick={toggleAll}>
              {allOpen ? "Collapse all" : "Expand all"}
            </Button>
          </div>

          {groupBy(visibleEntries).map(([group, groupEntries]) => (
            <section className="mt-5" key={group}>
              <div className="mb-2 flex flex-wrap items-baseline gap-2.5">
                <h2 className="m-0 text-sm font-semibold tracking-[0.06em] text-muted-foreground uppercase">{group}</h2>
                <span className="text-xs text-muted-foreground tabular-nums">
                  {groupEntries.length} setting{groupEntries.length === 1 ? "" : "s"}
                </span>
                {groupEntries.some((e) => e.restart) && (
                  <>
                    <span className="flex-1" />
                    <Badge variant="restart">takes effect on the next start</Badge>
                  </>
                )}
              </div>
              {groupEntries.some((e) => e.restart) && (
                <div className="rounded-[calc(var(--radius)-2px)] border border-border border-l-[3px] border-l-muted-foreground bg-[color-mix(in_srgb,var(--muted)_50%,transparent)] px-3 py-2 text-sm text-muted-foreground">
                  These are read once at startup or baked into a queue
                  definition. A write here is accepted and stored immediately
                  and changes nothing until the process restarts — which is
                  worse than a setting that cannot be changed at all, so the
                  group says so rather than each row repeating it.
                </div>
              )}
              <div className="overflow-hidden rounded-lg border border-border bg-card">
                {groupEntries.map((entry) => (
                  <SettingRow
                    key={entry.key}
                    entry={entry}
                    row={rows[entry.key] ?? { draft: "", busy: false, error: null }}
                    open={openKeys.has(entry.key)}
                    onOpenChange={(open) => toggleOpen(entry.key, open)}
                    onDraft={updateDraft}
                    onSave={save}
                    onSaveAllowed={saveAllowed}
                    onClear={clear}
                  />
                ))}
              </div>
            </section>
          ))}
        </>
      )}
    </div>
  );
}
