// The MCP client: the fetch calls and the wire types for /api/mcp/servers
// (docs/MCP.md). Kept as a module separate from the React component, like
// settings.ts and operations.ts — the component renders, this talks to the
// server, and the tests pin this module's wire shape without any DOM.
//
// The server owns the display value for a secret: an env or header value is
// masked to at most its last four characters (internal/redact.Secret, the
// same mask the settings screen renders), keys stay in the clear, and the
// full values never leave the process. On PATCH, a value sent as the empty
// string keeps the stored secret for that key, and a key left out of the
// env/headers map removes it — that asymmetry is how an operator edits a
// server without re-typing its API keys, and it lives entirely server-side;
// this module just carries the map through.

// MCPTool is one tool a server contributed at its last successful probe,
// mirroring internal/mcpclient's stored snapshot: the tool's own name, the
// mcp__<server>__<tool> name the model actually sees (docs/MCP.md
// "Naming"), and its description.
export interface MCPTool {
  name: string;
  qualified_name: string;
  description: string;
}

// MCPServer is one mcp_servers row over HTTP. transport picks which of the
// two connection shapes is live: stdio's command/args/env, or http's
// url/headers — the other half's fields are present but empty. tools is the
// snapshot from the last successful probe (empty until one has succeeded);
// tool_count mirrors tools.length so the screen can show a count without
// decoding the array when it only needs the number. probed_at is "" when the
// server has never probed successfully; probe_error is "" when the last
// probe (if any) succeeded, and non-empty when it failed — the two are
// independent, since a probe can fail after an earlier one succeeded, in
// which case probed_at still names that earlier success and probe_error
// names why the most recent attempt didn't replace it (docs/MCP.md
// "Probing").
export interface MCPServer {
  name: string;
  transport: "stdio" | "http";
  command: string;
  args: string[];
  env: Record<string, string>;
  url: string;
  headers: Record<string, string>;
  enabled: boolean;
  allow_readonly: boolean;
  tools: MCPTool[];
  tool_count: number;
  probed_at: string;
  probe_error: string;
  created_at: string;
  updated_at: string;
}

// MCPServerInput is the body POST /api/mcp/servers accepts: the connection
// shape plus the two flags, with no server-computed field (tools, the probe
// state, the timestamps) — those exist only once the server has created the
// row and, for tools, probed it.
export interface MCPServerInput {
  name: string;
  transport: "stdio" | "http";
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  enabled?: boolean;
  allow_readonly?: boolean;
}

// MCPServerPatch is the body PATCH /api/mcp/servers/{name} accepts: every
// field optional, name excluded (the path names the row and renaming is not
// offered). A field left out of the patch keeps its stored value — this is
// what makes the env/headers keep-or-remove convention above meaningful:
// omitting the whole map keeps every key as it was, sending a map applies
// the keep-empty-string / remove-when-absent rule per key within it.
export type MCPServerPatch = Partial<Omit<MCPServerInput, "name">>;

// listMCPServers fetches GET /api/mcp/servers: every configured server, env
// and header values masked.
export async function listMCPServers(): Promise<MCPServer[]> {
  const res = await fetch("/api/mcp/servers");
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as MCPServer[];
}

// createMCPServer adds a server via POST /api/mcp/servers. 201 with the row
// (unprobed — probed_at is "" until the operator refreshes or the server
// probes it on creation per docs/MCP.md "Probing"); 400 with a message the
// screen shows next to the field that failed; 409 when the name is already
// taken.
export async function createMCPServer(input: MCPServerInput): Promise<MCPServer> {
  const res = await fetch("/api/mcp/servers", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as MCPServer;
}

// updateMCPServer partially updates a server via PATCH
// /api/mcp/servers/{name}, returning the row as stored. Used both for the
// single-field enable/disable toggle and for the full edit form.
export async function updateMCPServer(name: string, patch: MCPServerPatch): Promise<MCPServer> {
  const res = await fetch(`/api/mcp/servers/${encodeURIComponent(name)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as MCPServer;
}

// deleteMCPServer removes a server via DELETE /api/mcp/servers/{name}. 204
// with no body.
export async function deleteMCPServer(name: string): Promise<void> {
  const res = await fetch(`/api/mcp/servers/${encodeURIComponent(name)}`, { method: "DELETE" });
  if (!res.ok) throw await apiError(res);
}

// refreshMCPServer probes a server via POST
// /api/mcp/servers/{name}/refresh and returns the row either way — a failed
// probe still comes back 200, with probe_error set and the old tools
// snapshot left alone (docs/MCP.md "Probing"). 503 means the harness has no
// MCP manager wired, which the caller shows as the server's own message.
export async function refreshMCPServer(name: string): Promise<MCPServer> {
  const res = await fetch(`/api/mcp/servers/${encodeURIComponent(name)}/refresh`, { method: "POST" });
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as MCPServer;
}

// apiError turns a non-2xx response into a readable Error, the same
// convention as settings.ts and operations.ts: every error the server writes
// carries {"error": "..."}, so the message can go straight to the screen
// instead of a failed write looking like a success.
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
