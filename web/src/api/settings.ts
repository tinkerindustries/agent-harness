// The settings client: the fetch calls and the wire type for the settings
// endpoints (docs/DESIGN.md §4.2, internal/httpapi/server.go). Kept as a
// module separate from the React component, like fold.ts and the stores
// beside it — the component renders, this talks to the server, and the
// tests pin this module's wire shape without any DOM.
//
// The server owns the display value: a secret key (deepseek.api_key,
// google.api_key) is masked to at most its last four characters and an
// unset key omits value, so the full value never leaves the process. This
// module deliberately has no way to read one back — the settings screen
// re-fetches rather than guessing, and there is no reveal path.

// SettingEntry is one row of GET /api/settings, mirroring
// internal/httpapi.settingEntry: the registry descriptor (group, type,
// default, description, secret, restart) plus the run's own state — whether
// it is set, whether the current value is an override or the default, and
// its display value. value is absent when the key is unset, and masked for
// a secret key. The registry is the source of truth; this module only
// carries what the screen needs to render it.
export interface SettingEntry {
  key: string;
  group: string;
  type: "string" | "integer" | "duration";
  default: string;
  description: string;
  secret: boolean;
  restart: boolean;
  set: boolean;
  override: boolean;
  value?: string;
}

// listSettings fetches GET /api/settings: every known key in registry
// order, grouped, each with its descriptor and set state.
export async function listSettings(): Promise<SettingEntry[]> {
  const res = await fetch("/api/settings");
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as SettingEntry[];
}

// setSetting writes key via PUT /api/settings/{key} with the value as the
// JSON body. The server demands Content-Type: application/json (415
// otherwise) and refuses a cross-origin Origin (403), so the write sends the
// header and the browser adds its own Origin, which is same-origin for a
// page served by the harness itself. A value the registry rejects comes back
// as a 400 whose message the screen shows next to the field — validation
// lives in Go, not here.
export async function setSetting(key: string, value: string): Promise<void> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ value }),
  });
  if (!res.ok) throw await apiError(res);
}

// deleteSetting unsets key via DELETE /api/settings/{key}, returning the
// setting to its default. It carries the same content-type requirement as
// PUT even though it has no body.
export async function deleteSetting(key: string): Promise<void> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
  });
  if (!res.ok) throw await apiError(res);
}

// apiError turns a non-2xx settings response into a readable Error. Every
// error the server writes carries {"error": "..."} — the 400 for an unknown
// key names the valid keys, the 400 for a rejected value names the registry
// bound, the 415 names the content-type rule, the 403 the origin rule — so
// the message can go straight to the screen next to the field instead of a
// failed write looking like a success.
async function apiError(res: Response): Promise<Error> {
  let message = `${res.status} ${res.statusText}`;
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) message = body.error;
  } catch {
    // A non-JSON error body falls back to the status line.
  }
  return new Error(message);
}
