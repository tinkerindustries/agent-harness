import { useCallback, useEffect, useState } from "react";
import { deleteSetting, listSettings, setSetting } from "../api/settings";
import type { SettingEntry } from "../api/settings";

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

// describe is display-only wording for the three known keys
// (internal/settings), keyed so a key added server-side still renders (its
// key alone) without the screen pretending to know what it is for.
const DESCRIPTIONS: Record<string, string> = {
  "deepseek.api_key": "DeepSeek API key — the harness's own account",
  "google.api_key": "Google API key — sent to Gemini by ReviewScreenshot",
  "google.vision_model": "Gemini model ReviewScreenshot sends screenshots to",
};

function describe(key: string): string {
  return DESCRIPTIONS[key] ?? key;
}

// The two secret keys are typed into a password field so what is being
// entered is not legible over a shoulder. This is input masking only — the
// display value beside the field is whatever mask the server decided.
const SECRET_KEYS: ReadonlySet<string> = new Set(["deepseek.api_key", "google.api_key"]);

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

  async function unset(key: string) {
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
        <button className="back-button" onClick={onBack}>
          ← sessions
        </button>
        <h1>Settings</h1>
      </header>
      {loadError && (
        <div className="settings-error settings-error-banner">
          could not load settings: {loadError}
        </div>
      )}
      {entries === null && !loadError && <p className="dim">Loading settings…</p>}
      {entries !== null &&
        entries.map((entry) => {
          const row = rows[entry.key] ?? { draft: "", busy: false, error: null };
          return (
            <div className="settings-row" key={entry.key}>
              <div className="settings-row-head">
                <span className="settings-key">{entry.key}</span>
                <span className="dim">{describe(entry.key)}</span>
              </div>
              <div className="settings-current">
                {entry.set ? (
                  <>
                    <span className="status-badge status-ok">set</span>
                    <span className="settings-value">{entry.value}</span>
                  </>
                ) : (
                  <span className="dim">not set</span>
                )}
              </div>
              <div className="settings-write">
                <input
                  className="settings-input"
                  type={SECRET_KEYS.has(entry.key) ? "password" : "text"}
                  placeholder={entry.set ? "replace current value" : "set a value"}
                  value={row.draft}
                  onChange={(ev) => updateDraft(entry.key, ev.target.value)}
                  disabled={row.busy}
                  spellCheck={false}
                  autoComplete="off"
                />
                <button
                  className="back-button"
                  onClick={() => save(entry.key)}
                  disabled={row.busy || row.draft === ""}
                >
                  Save
                </button>
                <button
                  className="back-button"
                  onClick={() => unset(entry.key)}
                  disabled={row.busy || !entry.set}
                >
                  Unset
                </button>
                {row.error && <span className="settings-error">{row.error}</span>}
              </div>
            </div>
          );
        })}
    </div>
  );
}
