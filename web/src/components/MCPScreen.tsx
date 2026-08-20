import { useCallback, useEffect, useState } from "react";
import { ArrowsClockwise, CaretRight, PencilSimple, Trash } from "@phosphor-icons/react";
import {
  createMCPServer,
  deleteMCPServer,
  listMCPServers,
  refreshMCPServer,
  updateMCPServer,
  type MCPServer,
  type MCPServerInput,
  type MCPServerPatch,
} from "../api/mcp";
import { parseMCPCommand, splitCommandLine } from "../api/mcpCommand";
import { buildKVPatch, formatArgs, probeStatus, targetLine, type KVRow } from "./mcpStatus";
import { useNow } from "../hooks";
import { cn } from "@/lib/utils";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Card, CardContent } from "./ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "./ui/collapsible";
import { Input } from "./ui/input";
import { Toggle } from "./ui/toggle";
import { ToggleGroup, ToggleGroupItem } from "./ui/toggle-group";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./ui/tooltip";
import { useNavRight } from "./TopNav";

// The MCP screen (docs/MCP.md): one card per configured server, an
// enable/disable toggle as the one-click primary action, and an add/edit
// form behind a paste box that reads a `claude mcp add ...` line, a bare
// command, or a bare URL (api/mcpCommand.ts parseMCPCommand). Follows the
// settings screen's discipline (web/CLAUDE.md "What that means for work
// here"): every write re-fetches rather than guessing at the new value,
// except the enable/disable toggle, which is explicitly optimistic — the
// operator asked for that action to feel instant — and rolls back on a
// failed write, showing the server's own message next to the row.
//
// Env and header values arrive from the server already masked
// (internal/redact.Secret); mcpStatus.ts's buildKVPatch is what lets an
// edit keep a secret's stored value without ever re-typing it.

const EXAMPLE_COMMAND = "claude mcp add --scope user blender -- uvx blender-mcp";

interface FormState {
  name: string;
  transport: "stdio" | "http";
  command: string;
  argsText: string;
  url: string;
  env: KVRow[];
  headers: KVRow[];
  allowReadonly: boolean;
}

function emptyForm(): FormState {
  return {
    name: "",
    transport: "stdio",
    command: "",
    argsText: "",
    url: "",
    env: [],
    headers: [],
    allowReadonly: false,
  };
}

function formFromServer(server: MCPServer): FormState {
  return {
    name: server.name,
    transport: server.transport,
    command: server.command,
    argsText: formatArgs(server.args),
    url: server.url,
    env: Object.entries(server.env).map(([key, value]) => ({ key, value, touched: false })),
    headers: Object.entries(server.headers).map(([key, value]) => ({ key, value, touched: false })),
    allowReadonly: server.allow_readonly,
  };
}

export function MCPScreen() {
  const [servers, setServers] = useState<MCPServer[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [busyRows, setBusyRows] = useState<Record<string, boolean>>({});
  const [rowErrors, setRowErrors] = useState<Record<string, string | null>>({});
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [openTools, setOpenTools] = useState<Set<string>>(new Set());
  const [refreshingAll, setRefreshingAll] = useState(false);
  // Relative "probed N minutes ago" text is coarse enough that a
  // once-a-minute tick is plenty — this is not the transcript's frame
  // budget problem (web/CLAUDE.md), just a handful of cards.
  const now = useNow(60_000);

  const refresh = useCallback(async () => {
    try {
      const next = await listMCPServers();
      setServers(next);
      setLoadError(null);
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function setBusy(name: string, busy: boolean) {
    setBusyRows((prev) => ({ ...prev, [name]: busy }));
  }

  function setRowError(name: string, message: string | null) {
    setRowErrors((prev) => ({ ...prev, [name]: message }));
  }

  // toggleEnabled is the primary action, and it is optimistic: the switch
  // flips the instant it's clicked, the PATCH runs behind it, and a failed
  // write flips it back and shows why — the one place on this screen that
  // does not simply re-fetch and wait.
  async function toggleEnabled(server: MCPServer) {
    const next = !server.enabled;
    setServers((prev) => prev && prev.map((s) => (s.name === server.name ? { ...s, enabled: next } : s)));
    setRowError(server.name, null);
    setBusy(server.name, true);
    try {
      await updateMCPServer(server.name, { enabled: next });
    } catch (err) {
      setServers((prev) => prev && prev.map((s) => (s.name === server.name ? { ...s, enabled: !next } : s)));
      setRowError(server.name, err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(server.name, false);
    }
  }

  async function refreshOne(name: string) {
    setBusy(name, true);
    setRowError(name, null);
    try {
      const updated = await refreshMCPServer(name);
      setServers((prev) => prev && prev.map((s) => (s.name === name ? updated : s)));
    } catch (err) {
      setRowError(name, err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(name, false);
    }
  }

  async function deleteOne(name: string) {
    setBusy(name, true);
    try {
      await deleteMCPServer(name);
      setConfirmDelete(null);
      await refresh();
    } catch (err) {
      setRowError(name, err instanceof Error ? err.message : String(err));
      setBusy(name, false);
    }
  }

  async function refreshAll() {
    if (!servers || refreshingAll) return;
    setRefreshingAll(true);
    // Best-effort across every server: one down server (a missing uvx, a
    // crashed process) must not stop the rest from refreshing, and the
    // per-card status is what shows which ones failed once the list
    // re-fetches.
    await Promise.allSettled(servers.map((s) => refreshMCPServer(s.name)));
    await refresh();
    setRefreshingAll(false);
  }

  useNavRight(
    <Button
      variant="outline"
      size="sm"
      onClick={refreshAll}
      disabled={refreshingAll || servers === null || servers.length === 0}
      title="Refresh every server"
    >
      <ArrowsClockwise className={refreshingAll ? "animate-spin" : undefined} />
      <span className="max-nav:sr-only">Refresh all</span>
    </Button>,
  );

  function toggleTools(name: string) {
    setOpenTools((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  }

  async function onSaved() {
    setShowAdd(false);
    setEditing(null);
    await refresh();
  }

  const enabledCount = servers?.filter((s) => s.enabled).length ?? 0;
  const totalTools = servers?.reduce((sum, s) => sum + s.tool_count, 0) ?? 0;

  return (
    <TooltipProvider>
      <div className="screen">
        {loadError && (
          <div className="mb-2 rounded-[calc(var(--radius)-4px)] border border-[var(--status-failed)] p-2 text-[0.85rem] text-[var(--status-failed)]">
            could not load MCP servers: {loadError}
          </div>
        )}
        {servers === null && !loadError && <p className="text-muted-foreground">Loading MCP servers…</p>}

        {servers !== null && (
          <>
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-[calc(var(--radius)-2px)] border border-border px-3 py-[7px] text-sm text-muted-foreground">
              <span>
                <b className="font-semibold text-foreground tabular-nums">{servers.length}</b> server
                {servers.length === 1 ? "" : "s"}
              </span>
              <span className="text-border">·</span>
              <span>
                <b className="font-semibold text-foreground tabular-nums">{enabledCount}</b> enabled
              </span>
              <span className="text-border">·</span>
              <span>
                <b className="font-semibold text-foreground tabular-nums">{totalTools}</b> tool
                {totalTools === 1 ? "" : "s"} available to every run
              </span>
            </div>

            <div className="mt-3 mb-3 flex items-center justify-end">
              <Button
                variant={showAdd ? "outline" : "default"}
                size="sm"
                onClick={() => {
                  setEditing(null);
                  setShowAdd((v) => !v);
                }}
              >
                {showAdd ? "Cancel" : "Add server"}
              </Button>
            </div>

            {showAdd && <ServerForm mode="add" onCancel={() => setShowAdd(false)} onSaved={onSaved} />}

            {servers.length === 0 && !showAdd ? (
              <EmptyState onAdd={() => setShowAdd(true)} />
            ) : (
              <div className="flex flex-col gap-3">
                {servers.map((server) =>
                  editing === server.name ? (
                    <ServerForm
                      key={server.name}
                      mode="edit"
                      server={server}
                      onCancel={() => setEditing(null)}
                      onSaved={onSaved}
                    />
                  ) : (
                    <ServerCard
                      key={server.name}
                      server={server}
                      now={now}
                      busy={!!busyRows[server.name]}
                      error={rowErrors[server.name] ?? null}
                      toolsOpen={openTools.has(server.name)}
                      confirmingDelete={confirmDelete === server.name}
                      onToggleEnabled={() => toggleEnabled(server)}
                      onToggleTools={() => toggleTools(server.name)}
                      onRefresh={() => refreshOne(server.name)}
                      onEdit={() => {
                        setShowAdd(false);
                        setEditing(server.name);
                      }}
                      onDeleteClick={() => setConfirmDelete(server.name)}
                      onDeleteConfirm={() => deleteOne(server.name)}
                      onDeleteCancel={() => setConfirmDelete(null)}
                    />
                  ),
                )}
              </div>
            )}
          </>
        )}
      </div>
    </TooltipProvider>
  );
}

function EmptyState({ onAdd }: { onAdd: () => void }) {
  return (
    <div className="flex flex-col items-start gap-2.5 rounded-lg border border-dashed border-border px-4 py-6 text-sm text-muted-foreground">
      <p className="m-0 max-w-[60ch]">
        No MCP servers configured. Every session's tool array stays exactly what it is today until one is added
        (docs/MCP.md).
      </p>
      <p className="m-0 max-w-[60ch]">
        Add one by pasting a command like <code className="rounded bg-muted px-1 font-mono">{EXAMPLE_COMMAND}</code>{" "}
        into the add form, or fill in the fields by hand.
      </p>
      <Button size="sm" onClick={onAdd}>
        Add server
      </Button>
    </div>
  );
}

interface ServerCardProps {
  server: MCPServer;
  now: number;
  busy: boolean;
  error: string | null;
  toolsOpen: boolean;
  confirmingDelete: boolean;
  onToggleEnabled: () => void;
  onToggleTools: () => void;
  onRefresh: () => void;
  onEdit: () => void;
  onDeleteClick: () => void;
  onDeleteConfirm: () => void;
  onDeleteCancel: () => void;
}

function ServerCard({
  server,
  now,
  busy,
  error,
  toolsOpen,
  confirmingDelete,
  onToggleEnabled,
  onToggleTools,
  onRefresh,
  onEdit,
  onDeleteClick,
  onDeleteConfirm,
  onDeleteCancel,
}: ServerCardProps) {
  const status = probeStatus(server, now);

  return (
    <Card className={cn("gap-3 py-4", !server.enabled && "opacity-70")}>
      <CardContent className="flex flex-col gap-2.5 px-4">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-base font-semibold">{server.name}</span>
          <Badge variant={status.variant}>{status.label}</Badge>
          {server.allow_readonly && <Badge variant="outline">READONLY OK</Badge>}
          <span className="flex-1" />
          <Toggle
            pressed={server.enabled}
            onPressedChange={onToggleEnabled}
            disabled={busy}
            variant="outline"
            size="sm"
            className="data-[state=on]:border-[var(--status-done)] data-[state=on]:bg-[var(--status-done-bg)] data-[state=on]:text-[var(--status-done)]"
            aria-label={server.enabled ? `Disable ${server.name}` : `Enable ${server.name}`}
          >
            {server.enabled ? "Enabled" : "Disabled"}
          </Toggle>
        </div>

        <code className="w-fit max-w-full truncate rounded bg-muted px-1.5 py-0.5 font-mono text-sm text-foreground">
          {targetLine(server)}
        </code>

        <Tooltip>
          <TooltipTrigger asChild>
            <span className="w-fit cursor-default text-xs text-muted-foreground">{status.detail}</span>
          </TooltipTrigger>
          <TooltipContent>
            {server.probed_at !== "" ? new Date(server.probed_at).toLocaleString() : "never probed"}
          </TooltipContent>
        </Tooltip>

        <Collapsible open={toolsOpen} onOpenChange={onToggleTools}>
          <CollapsibleTrigger asChild>
            <button
              type="button"
              className="flex w-fit cursor-pointer items-center gap-1 border-0 bg-transparent p-0 text-xs text-muted-foreground hover:text-foreground"
            >
              <CaretRight className={cn("h-3 w-3 [transition:transform_var(--dur-caret)_var(--ease)]", toolsOpen && "rotate-90")} />
              {/* Not the tool count again — the status line above already
                  carries it, and a card that says "25 tools" twice reads
                  like one of them means something else. */}
              {toolsOpen ? "Hide tools" : "Show tools"}
            </button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            <ul className="m-0 mt-2 flex list-none flex-col gap-1.5 border-l border-border py-0 pl-3">
              {server.tools.length === 0 ? (
                <li className="text-xs text-muted-foreground">No tools yet — refresh to probe the server.</li>
              ) : (
                server.tools.map((tool) => (
                  <li key={tool.qualified_name} className="flex flex-col gap-0.5">
                    <code className="w-fit rounded bg-muted px-1 font-mono text-xs">{tool.qualified_name}</code>
                    {tool.description && (
                      <span className="text-xs text-muted-foreground">{tool.description}</span>
                    )}
                  </li>
                ))
              )}
            </ul>
          </CollapsibleContent>
        </Collapsible>

        <div className="flex flex-wrap items-center gap-2 pt-1">
          {confirmingDelete ? (
            <div className="flex flex-col items-start gap-[6px] text-sm text-foreground max-phone:w-full">
              <span>
                Delete server <code className="rounded bg-muted px-1 font-mono">{server.name}</code>? Its tool
                snapshot goes with it — this cannot be undone.
              </span>
              <div className="flex gap-2">
                <Button variant="destructive" size="sm" onClick={onDeleteConfirm} disabled={busy}>
                  <Trash />
                  Delete
                </Button>
                <Button variant="outline" size="sm" onClick={onDeleteCancel} disabled={busy}>
                  Cancel
                </Button>
              </div>
            </div>
          ) : (
            <>
              <Button variant="outline" size="sm" onClick={onRefresh} disabled={busy}>
                <ArrowsClockwise className={busy ? "animate-spin" : undefined} />
                {busy ? "Refreshing…" : "Refresh"}
              </Button>
              <Button variant="outline" size="sm" onClick={onEdit} disabled={busy}>
                <PencilSimple />
                Edit
              </Button>
              {/* Outline, not destructive: this click only opens the
                  confirmation below it, so shouting here puts the loudest
                  control on the card next to the two ordinary ones. The
                  confirm button that actually deletes is the destructive
                  one. */}
              <Button variant="outline" size="sm" onClick={onDeleteClick} disabled={busy}>
                <Trash />
                Delete
              </Button>
            </>
          )}
          {error && <span className="text-xs font-mono text-[var(--status-failed)] basis-full">{error}</span>}
        </div>
      </CardContent>
    </Card>
  );
}

interface KeyValueRowsProps {
  label: string;
  rows: KVRow[];
  onChange: (rows: KVRow[]) => void;
  keyPlaceholder: string;
  hint?: string;
}

function KeyValueRows({ label, rows, onChange, keyPlaceholder, hint }: KeyValueRowsProps) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">{label}</span>
      {hint && <p className="m-0 text-xs text-muted-foreground">{hint}</p>}
      {rows.map((row, i) => (
        <div className="flex items-center gap-2" key={i}>
          <Input
            className="flex-1 font-mono text-sm"
            placeholder={keyPlaceholder}
            value={row.key}
            onChange={(e) => onChange(rows.map((r, j) => (j === i ? { ...r, key: e.target.value } : r)))}
            spellCheck={false}
          />
          <Input
            className="flex-[2] font-mono text-sm"
            placeholder="value"
            value={row.value}
            onChange={(e) =>
              onChange(rows.map((r, j) => (j === i ? { ...r, value: e.target.value, touched: true } : r)))
            }
            spellCheck={false}
          />
          <Button variant="outline" size="sm" onClick={() => onChange(rows.filter((_, j) => j !== i))}>
            Remove
          </Button>
        </div>
      ))}
      <Button
        variant="outline"
        size="sm"
        className="w-fit"
        onClick={() => onChange([...rows, { key: "", value: "", touched: true }])}
      >
        Add row
      </Button>
    </div>
  );
}

interface ServerFormProps {
  mode: "add" | "edit";
  server?: MCPServer;
  onCancel: () => void;
  onSaved: () => void | Promise<void>;
}

function ServerForm({ mode, server, onCancel, onSaved }: ServerFormProps) {
  const [form, setForm] = useState<FormState>(() => (server ? formFromServer(server) : emptyForm()));
  const [pasteText, setPasteText] = useState("");
  const [pasteError, setPasteError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function updateField<K extends keyof FormState>(key: K, value: FormState[K]) {
    setForm((prev) => ({ ...prev, [key]: value }));
  }

  function applyPaste() {
    const result = parseMCPCommand(pasteText);
    if ("error" in result) {
      setPasteError(result.error);
      return;
    }
    setPasteError(null);
    setForm((prev) => ({
      ...prev,
      name: result.name,
      transport: result.transport,
      command: result.command,
      argsText: formatArgs(result.args),
      url: result.url,
      env: Object.entries(result.env).map(([key, value]) => ({ key, value, touched: true })),
      headers: Object.entries(result.headers).map(([key, value]) => ({ key, value, touched: true })),
    }));
  }

  async function submit() {
    if (busy) return;
    setError(null);

    const name = form.name.trim();
    if (mode === "add" && name === "") {
      setError("Name the server.");
      return;
    }
    if (form.transport === "stdio" && form.command.trim() === "") {
      setError("A stdio server needs a command.");
      return;
    }
    if (form.transport === "http" && form.url.trim() === "") {
      setError("An http server needs a URL.");
      return;
    }

    let args: string[] = [];
    if (form.transport === "stdio") {
      const split = splitCommandLine(form.argsText);
      if (split === null) {
        setError("Arguments: unterminated quote.");
        return;
      }
      args = split;
    }

    setBusy(true);
    try {
      if (mode === "add") {
        const input: MCPServerInput = {
          name,
          transport: form.transport,
          command: form.transport === "stdio" ? form.command.trim() : "",
          args: form.transport === "stdio" ? args : [],
          env: form.transport === "stdio" ? buildKVPatch(form.env, "add") : {},
          url: form.transport === "http" ? form.url.trim() : "",
          headers: form.transport === "http" ? buildKVPatch(form.headers, "add") : {},
          // A new server starts enabled — adding one is rare, turning one
          // on and off is not (the toggle on the card is what carries that
          // weight after creation), so the form offers no control for this
          // and the create call states it explicitly rather than leaving it
          // to whatever a server-side default for an absent field would be.
          enabled: true,
          allow_readonly: form.allowReadonly,
        };
        await createMCPServer(input);
      } else if (server) {
        const patch: MCPServerPatch = {
          transport: form.transport,
          command: form.transport === "stdio" ? form.command.trim() : "",
          args: form.transport === "stdio" ? args : [],
          env: form.transport === "stdio" ? buildKVPatch(form.env, "edit") : {},
          url: form.transport === "http" ? form.url.trim() : "",
          headers: form.transport === "http" ? buildKVPatch(form.headers, "edit") : {},
          allow_readonly: form.allowReadonly,
        };
        await updateMCPServer(server.name, patch);
      }
      await onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card className="mb-4 gap-3 py-4">
      <CardContent className="flex flex-col gap-3.5 px-4">
        <div className="flex items-center gap-2">
          <span className="flex-1 font-semibold">{mode === "add" ? "Add server" : `Edit ${server?.name}`}</span>
          <Button variant="outline" size="sm" onClick={onCancel}>
            Cancel
          </Button>
        </div>

        {mode === "add" && (
          <div className="flex flex-col gap-1.5">
            <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Paste a command</span>
            <div className="flex items-center gap-2">
              <Input
                className="flex-1 font-mono text-sm"
                placeholder={EXAMPLE_COMMAND}
                value={pasteText}
                onChange={(e) => setPasteText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    applyPaste();
                  }
                }}
                spellCheck={false}
              />
              <Button variant="outline" size="sm" onClick={applyPaste} disabled={pasteText.trim() === ""}>
                Fill fields
              </Button>
            </div>
            {pasteError && <span className="text-xs font-mono text-[var(--status-failed)]">{pasteError}</span>}
            <p className="m-0 text-xs text-muted-foreground">
              Recognises a full <code className="rounded bg-muted px-1 font-mono">claude mcp add …</code> line, a
              bare command (<code className="rounded bg-muted px-1 font-mono">uvx blender-mcp</code>), or a bare
              URL. The fields below stay editable either way.
            </p>
          </div>
        )}

        <div className="flex flex-wrap items-end gap-3.5 max-touch:flex-col max-touch:items-stretch">
          <label className="flex flex-1 flex-col gap-1">
            <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Name</span>
            <Input
              className="font-mono text-sm"
              value={form.name}
              onChange={(e) => updateField("name", e.target.value)}
              disabled={mode === "edit"}
              placeholder="blender"
              spellCheck={false}
            />
          </label>
          <div className="flex flex-col gap-1">
            <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Transport</span>
            <ToggleGroup
              type="single"
              value={form.transport}
              onValueChange={(v) => v && updateField("transport", v as "stdio" | "http")}
            >
              <ToggleGroupItem value="stdio">stdio</ToggleGroupItem>
              <ToggleGroupItem value="http">http</ToggleGroupItem>
            </ToggleGroup>
          </div>
        </div>

        {form.transport === "stdio" ? (
          <>
            <div className="flex flex-wrap gap-3.5 max-touch:flex-col">
              <label className="flex min-w-[160px] flex-1 flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Command</span>
                <Input
                  className="font-mono text-sm"
                  value={form.command}
                  onChange={(e) => updateField("command", e.target.value)}
                  placeholder="uvx"
                  spellCheck={false}
                />
              </label>
              <label className="flex min-w-[240px] flex-[2] flex-col gap-1">
                <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">Arguments</span>
                <Input
                  className="font-mono text-sm"
                  value={form.argsText}
                  onChange={(e) => updateField("argsText", e.target.value)}
                  placeholder="blender-mcp"
                  spellCheck={false}
                />
              </label>
            </div>
            <KeyValueRows
              label="Environment"
              rows={form.env}
              onChange={(rows) => updateField("env", rows)}
              keyPlaceholder="KEY"
              hint={
                mode === "edit" && form.env.length > 0
                  ? "Values are shown masked. Leave a row untouched to keep its stored secret."
                  : undefined
              }
            />
          </>
        ) : (
          <>
            <label className="flex flex-col gap-1">
              <span className="text-xs tracking-[0.04em] text-muted-foreground uppercase">URL</span>
              <Input
                className="font-mono text-sm"
                value={form.url}
                onChange={(e) => updateField("url", e.target.value)}
                placeholder="https://example.com/mcp"
                spellCheck={false}
              />
            </label>
            <KeyValueRows
              label="Headers"
              rows={form.headers}
              onChange={(rows) => updateField("headers", rows)}
              keyPlaceholder="Header-Name"
              hint={
                mode === "edit" && form.headers.length > 0
                  ? "Values are shown masked. Leave a row untouched to keep its stored secret."
                  : undefined
              }
            />
          </>
        )}

        <label className="flex items-start gap-2 text-sm">
          <input
            type="checkbox"
            className="mt-1"
            checked={form.allowReadonly}
            onChange={(e) => updateField("allowReadonly", e.target.checked)}
          />
          <span className="flex flex-col gap-0.5">
            <span>Allow in read-only sessions</span>
            <span className="text-xs text-muted-foreground">
              MCP tools reach outside the workspace, so a readonly run cannot call this server&rsquo;s tools unless
              this is on (docs/MCP.md "Permissions").
            </span>
          </span>
        </label>

        <div className="flex flex-wrap items-center gap-3">
          <Button onClick={submit} disabled={busy}>
            {busy ? "Saving…" : mode === "add" ? "Add server" : "Save changes"}
          </Button>
          {error && <span className="text-xs font-mono text-[var(--status-failed)] basis-full">{error}</span>}
        </div>
      </CardContent>
    </Card>
  );
}
