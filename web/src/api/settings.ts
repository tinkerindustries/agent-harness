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
// internal/httpapi.settingEntry: the key, whether it is set, and its display
// value. value is absent when the key is unset, and masked for a secret key
// (settings.IsSecretKey). That is all the display information the settings
// screen has or needs.
export interface SettingEntry {
  key: string;
  set: boolean;
  value?: string;
}

// listSettings fetches GET /api/settings: every known key in
// settings.ValidKeys order, each with its set state and display value.
export async function listSettings(): Promise<SettingEntry[]> {
  const res = await fetch("/api/settings");
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as SettingEntry[];
}

// setSetting writes key via PUT /api/settings/{key} with the value as the
// JSON body. The server demands Content-Type: application/json (415
// otherwise) and refuses a cross-origin Origin (403), so the write sends the
// header and the browser adds its own Origin, which is same-origin for a
// page served by the harness itself.
export async function setSetting(key: string, value: string): Promise<void> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ value }),
  });
  if (!res.ok) throw await apiError(res);
}

// deleteSetting unsets key via DELETE /api/settings/{key}. It carries the
// same content-type requirement as PUT even though it has no body.
export async function deleteSetting(key: string): Promise<void> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
  });
  if (!res.ok) throw await apiError(res);
}

// apiError turns a non-2xx settings response into a readable Error. Every
// error the server writes carries {"error": "..."} — the 400 for an unknown
// key names the valid keys, the 415 names the content-type rule, the 403 the
// origin rule — so the message can go straight to the screen next to the
// field instead of a failed write looking like a success.
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
