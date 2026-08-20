// Pure display logic for the MCP screen (web/src/components/MCPScreen.tsx),
// kept in its own module and tested without a DOM the way statusBadge.ts and
// sessionListTitle.ts are (TESTING.md, web/CLAUDE.md): status derivation,
// formatting a command line back from {command, args}, and the "probed 4
// minutes ago" relative time text.

import type { VariantProps } from "class-variance-authority";
import type { MCPServer } from "../api/mcp";
import { badgeVariants } from "./ui/badge";

type BadgeVariant = NonNullable<VariantProps<typeof badgeVariants>["variant"]>;

// quoteIfNeeded wraps an argument in double quotes when it contains
// whitespace, escaping any double quote already inside it — the inverse of
// api/mcpCommand.ts's tokenizer, so a command round-trips through the paste
// box and back.
function quoteIfNeeded(arg: string): string {
  if (arg === "" || /\s/.test(arg)) {
    return `"${arg.replace(/"/g, '\\"')}"`;
  }
  return arg;
}

// formatArgs renders just the argument list, space-separated and re-quoted
// only where needed — the add/edit form's Arguments field is seeded from
// this when it opens pre-filled on an existing stdio server.
export function formatArgs(args: string[]): string {
  return args.map(quoteIfNeeded).join(" ");
}

// formatCommandLine renders {command, args} the way a server's card shows
// the command it runs: the executable and its arguments, space-separated,
// an argument re-quoted only if it needs it to read back unambiguously.
export function formatCommandLine(command: string, args: string[]): string {
  const rest = formatArgs(args);
  return rest === "" ? quoteIfNeeded(command) : `${quoteIfNeeded(command)} ${rest}`;
}

// targetLine is the one line a server's card shows under its name: the URL
// for an http server, the command line for a stdio one.
export function targetLine(server: Pick<MCPServer, "transport" | "command" | "args" | "url">): string {
  return server.transport === "http" ? server.url : formatCommandLine(server.command, server.args);
}

// relativeTime renders an ISO timestamp as "4 minutes ago" text, the probe
// screen's version of formatDuration in api/operations.ts (that one measures
// a duration already known to be quiet; this one measures from now). "" or
// an unparseable timestamp both mean "never" — the caller decides whether
// that is worth showing at all (probeStatus below treats an empty probed_at
// as its own "never probed" state rather than calling this).
export function relativeTime(iso: string, nowMs: number): string {
  if (iso === "") return "never";
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return iso;
  const diffMs = Math.max(0, nowMs - then);
  const sec = Math.floor(diffMs / 1000);
  if (sec < 5) return "just now";
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min} minute${min === 1 ? "" : "s"} ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr} hour${hr === 1 ? "" : "s"} ago`;
  const day = Math.floor(hr / 24);
  return `${day} day${day === 1 ? "" : "s"} ago`;
}

// KVRow is one row of the add/edit form's env or headers editor: a key, a
// value, and whether the value has been typed into since the row was seeded
// — false only for a row seeded from an existing server's masked value that
// the operator has not touched.
export interface KVRow {
  key: string;
  value: string;
  touched: boolean;
}

// buildKVPatch turns the add/edit form's key/value rows into the map a
// write sends, applying the docs/MCP.md keep-or-remove convention for
// env/headers on a PATCH: a row deleted from the list is simply absent from
// the map (removes the key), and an edit-mode row that still carries its
// masked, never-edited display value sends the empty string instead (keeps
// the stored secret) — that pairing is how an operator edits a server
// without re-typing its API keys. mode "add" has no stored secret to keep,
// so every row's current value goes through as typed regardless of touched.
// A row with a blank key is dropped: there is nothing to key the write on.
export function buildKVPatch(rows: KVRow[], mode: "add" | "edit"): Record<string, string> {
  const map: Record<string, string> = {};
  for (const row of rows) {
    const key = row.key.trim();
    if (key === "") continue;
    map[key] = mode === "edit" && !row.touched ? "" : row.value;
  }
  return map;
}

export interface ProbeStatus {
  label: string;
  variant: BadgeVariant;
  // detail is the status line's body text: the tool count and when it was
  // probed for a healthy server, the server's own probe_error verbatim for a
  // failed one (docs/MCP.md "Probing" — that message is how an operator
  // finds out uvx is missing or the server crashed on startup), and a plain
  // nudge to refresh for a server that has never probed successfully.
  detail: string;
}

// probeStatus derives the status line and its badge from a server row. A
// non-empty probe_error always wins, even over a probed_at that names an
// earlier success — the docs/MCP.md asymmetry ("tools_json is only ever
// written by a successful probe, and a failed probe writes probe_error and
// leaves the snapshot alone") means the two can disagree, and it is the
// most recent attempt an operator needs to see.
export function probeStatus(
  server: Pick<MCPServer, "probe_error" | "probed_at" | "tool_count">,
  nowMs: number,
): ProbeStatus {
  if (server.probe_error !== "") {
    return { label: "ERROR", variant: "failed", detail: server.probe_error };
  }
  if (server.probed_at === "") {
    return { label: "NEVER PROBED", variant: "outline", detail: "Never probed — refresh to connect." };
  }
  const tools = `${server.tool_count} tool${server.tool_count === 1 ? "" : "s"}`;
  return {
    label: "OK",
    variant: "done",
    detail: `${tools} · probed ${relativeTime(server.probed_at, nowMs)}`,
  };
}
