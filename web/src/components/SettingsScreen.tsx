import { useCallback, useEffect, useState } from "react";
import { deleteSetting, listSettings, setSetting } from "../api/settings";
import type { SettingEntry } from "../api/settings";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Input } from "./ui/input";

interface Props {
  onBack: () => void;
}

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

export function SettingsScreen({ onBack }: Props) {
  const [entries, setEntries] = useState<SettingEntry[] | null>(null);
  const [rows, setRows] = useState<Record<string, RowState>>({});
  const [loadError, setLoadError] = useState<string | null>(null);

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
        for (const entry of next) merged[entry.key] = prev[entry.key] ?? { draft: "", busy: false, error: null };
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
        [key]: { ...prev[key], busy: false, error: err instanceof Error ? err.message : String(err) },
      }));
    }
  }

  async function resetToDefault(key: string) {
    setRows((prev) => ({ ...prev, [key]: { ...prev[key], busy: true, error: null } }));
    try {
      await deleteSetting(key);
      await refresh();
      setRows((prev) => ({ ...prev, [key]: { ...prev[key], busy: false } }));
    } catch (err) {
      setRows((prev) => ({
        ...prev,
        [key]: { ...prev[key], busy: false, error: err instanceof Error ? err.message : String(err) },
      }));
    }
  }

  return (
    <div className="screen">
      <header className="screen-header">
        <Button variant="outline" size="sm" onClick={onBack}>
          ← sessions
        </Button>
        <h1>Settings</h1>
      </header>
      {loadError && (
        <div className="settings-error settings-error-banner">
          could not load settings: {loadError}
        </div>
      )}
      {entries === null && !loadError && <p className="dim">Loading settings…</p>}
      {entries !== null &&
        groupBy(entries).map(([group, groupEntries]) => (
          <section className="settings-group" key={group}>
            <h2 className="settings-group-heading">{group}</h2>
            {groupEntries.map((entry) => {
              const row = rows[entry.key] ?? { draft: "", busy: false, error: null };
              const placeholder = entry.set ? "replace current value" : entry.default;
              return (
                <div className="settings-row" key={entry.key}>
                  <div className="settings-row-head">
                    <span className="settings-key">{entry.key}</span>
                    {entry.restart && <Badge variant="restart">restart</Badge>}
                    <span className="dim">{entry.description}</span>
                  </div>
                  <div className="settings-current">
                    {entry.set ? (
                      <>
                        <Badge variant={entry.override ? "running" : "done"}>
                          {entry.override ? "override" : "default"}
                        </Badge>
                        <span className="settings-value">{entry.value}</span>
                        {entry.override && <span className="dim">default {entry.default}</span>}
                      </>
                    ) : (
                      <>
                        <Badge variant="done">default</Badge>
                        <span className="dim">not set — default {entry.default} applies</span>
                      </>
                    )}
                  </div>
                  <div className="settings-write">
                    <Input
                      className="settings-input"
                      type={inputType(entry)}
                      placeholder={placeholder}
                      value={row.draft}
                      onChange={(ev) => updateDraft(entry.key, ev.target.value)}
                      disabled={row.busy}
                      spellCheck={false}
                      autoComplete="off"
                    />
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => save(entry.key)}
                      disabled={row.busy || row.draft === ""}
                    >
                      Save
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => resetToDefault(entry.key)}
                      disabled={row.busy || !entry.set}
                      title="delete the stored value so the registry default applies"
                    >
                      reset to default
                    </Button>
                    {row.error && <span className="settings-error">{row.error}</span>}
                  </div>
                </div>
              );
            })}
          </section>
        ))}
    </div>
  );
}
