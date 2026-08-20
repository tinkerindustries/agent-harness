// parseMCPCommand: the add form's paste box turned into form fields
// (docs/MCP.md, web/CLAUDE.md "MCP servers"). This is pure logic with no
// fetch and no DOM — it never talks to the server and never validates
// against anything the server itself enforces (the name grammar is
// mirrored here only so the form's fields start out already valid, not as a
// second source of truth: a name this module accepts can still come back as
// a 400 from POST /api/mcp/servers, and that message wins).
//
// The motivating case is a line copied straight out of Claude Code's own
// docs or terminal history:
//
//   claude mcp add --scope user blender -- uvx blender-mcp
//
// which this harness has no use for as a command — its own config is
// global, not scoped per-project or per-user (docs/MCP.md "Configuration is
// global and lives in the database") — but which is exactly the shape an
// operator has sitting in a README or a shell history, so the paste box
// reads it directly rather than asking them to translate it into fields by
// hand.

// SERVER_NAME_RE mirrors the grammar internal/mcpclient validates server
// names against (docs/MCP.md "Naming"): lowercase, starts with a letter or
// digit, up to 32 characters total.
const SERVER_NAME_RE = /^[a-z0-9][a-z0-9_-]{0,31}$/;

export interface ParsedMCPServer {
  name: string;
  transport: "stdio" | "http";
  command: string;
  args: string[];
  env: Record<string, string>;
  url: string;
  headers: Record<string, string>;
}

export interface ParseError {
  error: string;
}

// tokenize splits a pasted line into shell-like words: whitespace-separated,
// with single or double quotes taken literally (no nesting, no backslash
// escapes) so `-e "KEY=a value"` keeps its value as one token. Returns null
// for a line with an unterminated quote.
function tokenize(input: string): string[] | null {
  const tokens: string[] = [];
  let i = 0;
  const n = input.length;
  while (i < n) {
    while (i < n && /\s/.test(input[i])) i++;
    if (i >= n) break;
    let token = "";
    let quote: string | null = null;
    while (i < n) {
      const c = input[i];
      if (quote) {
        if (c === quote) {
          quote = null;
          i++;
        } else {
          token += c;
          i++;
        }
      } else if (c === '"' || c === "'") {
        quote = c;
        i++;
      } else if (/\s/.test(c)) {
        break;
      } else {
        token += c;
        i++;
      }
    }
    if (quote) return null;
    tokens.push(token);
  }
  return tokens;
}

// sanitizeName turns arbitrary text into something inside SERVER_NAME_RE:
// strip an npm-style scope (@modelcontextprotocol/server-filesystem →
// server-filesystem), strip a leading "server-" (a near-universal package
// naming convention for MCP servers, → filesystem), lowercase, replace every
// character the grammar rejects with "-", drop any leading run of
// characters that can't start the name, and cap the length. Idempotent on
// text that already matches the grammar, so it is applied uniformly to an
// inferred name and an explicitly typed one alike.
export function sanitizeName(raw: string): string {
  let s = raw.trim().toLowerCase();
  s = s.replace(/^@[^/]+\//, "");
  s = s.replace(/^server-/, "");
  s = s.replace(/[^a-z0-9_-]/g, "-");
  s = s.replace(/^[^a-z0-9]+/, "");
  return s.slice(0, 32);
}

// splitCommandLine tokenizes a single line the same way parseMCPCommand's
// internals do — quotes respected, no shell expansion, no flag or "claude
// mcp add" handling — for the add/edit form's plain Arguments field, which
// needs the same splitting without going through the whole paste-box
// parser. null means an unterminated quote.
export function splitCommandLine(text: string): string[] | null {
  return tokenize(text);
}

export function parseMCPCommand(input: string): ParsedMCPServer | ParseError {
  const trimmed = input.trim();
  if (trimmed === "") return { error: "Paste a command to parse." };

  const tokens = tokenize(trimmed);
  if (tokens === null) return { error: "Unterminated quote." };
  if (tokens.length === 0) return { error: "Paste a command to parse." };

  // A bare URL with nothing else on the line is an http server named after
  // its host.
  if (tokens.length === 1 && /^https?:\/\//i.test(tokens[0])) {
    return parseBareUrl(tokens[0]);
  }

  if (tokens[0] === "claude" && tokens[1] === "mcp" && tokens[2] === "add") {
    return parseClaudeMcpAdd(tokens.slice(3));
  }

  return parseBareCommand(tokens);
}

function finalizeName(raw: string, source: string): ParsedMCPServer | ParseError | string {
  const name = sanitizeName(raw);
  if (!SERVER_NAME_RE.test(name)) {
    return {
      error: `Could not turn "${source}" into a server name — name it explicitly with claude mcp add.`,
    };
  }
  return name;
}

function parseBareUrl(url: string): ParsedMCPServer | ParseError {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return { error: `"${url}" is not a valid URL.` };
  }
  const host = parsed.hostname.split(".")[0] ?? "";
  const named = finalizeName(host, url);
  if (typeof named !== "string") return named;
  return { name: named, transport: "http", command: "", args: [], env: {}, url, headers: {} };
}

// parseBareCommand handles a plain command line with no "claude mcp add"
// prefix — uvx blender-mcp, npx -y @modelcontextprotocol/server-filesystem
// /tmp — as a stdio server. The name comes from the first non-flag argument
// after the runner (skipping a runner flag like npx's -y), through the same
// sanitizeName the explicit-name path uses, falling back to the runner
// itself when there is no such argument.
function parseBareCommand(tokens: string[]): ParsedMCPServer | ParseError {
  const command = tokens[0];
  const args = tokens.slice(1);
  if (!command) return { error: "Paste a command to parse." };
  const nameSource = args.find((a) => !a.startsWith("-")) ?? command;
  const named = finalizeName(nameSource, nameSource);
  if (typeof named !== "string") return named;
  return { name: named, transport: "stdio", command, args, env: {}, url: "", headers: {} };
}

// parseClaudeMcpAdd handles the tokens after "claude mcp add": flags
// (--scope, --transport, -e/--env, -H/--header, repeatable), the server
// name, then either "-- <command> [args...]" for stdio or a trailing URL for
// http.
function parseClaudeMcpAdd(tokens: string[]): ParsedMCPServer | ParseError {
  const env: Record<string, string> = {};
  const headers: Record<string, string> = {};
  let transport: "stdio" | "http" | null = null;
  const positionals: string[] = [];
  let i = 0;
  let sawSeparator = false;

  while (i < tokens.length) {
    const t = tokens[i];
    if (t === "--") {
      sawSeparator = true;
      i++;
      break;
    }
    if (t === "--scope") {
      // This harness's MCP config is global (docs/MCP.md "Configuration is
      // global and lives in the database") — there is no per-project or
      // per-user scope to honour, so --scope and its value are simply
      // dropped rather than rejected: a pasted command that names a scope
      // is still the same server everywhere this config applies.
      if (tokens[i + 1] === undefined) return { error: "--scope needs a value." };
      i += 2;
      continue;
    }
    if (t === "--transport") {
      const v = tokens[i + 1];
      if (v === undefined) return { error: "--transport needs a value." };
      if (v === "sse") {
        return { error: "The sse transport is not supported here — only stdio and http are." };
      }
      if (v !== "stdio" && v !== "http") return { error: `Unknown transport "${v}".` };
      transport = v;
      i += 2;
      continue;
    }
    if (t === "-e" || t === "--env") {
      const v = tokens[i + 1];
      if (v === undefined) return { error: `${t} needs a KEY=value argument.` };
      const eq = v.indexOf("=");
      if (eq < 0) return { error: `${t} value "${v}" is not KEY=value.` };
      env[v.slice(0, eq)] = v.slice(eq + 1);
      i += 2;
      continue;
    }
    if (t === "-H" || t === "--header") {
      const v = tokens[i + 1];
      if (v === undefined) return { error: `${t} needs a "Key: value" argument.` };
      const colon = v.indexOf(":");
      if (colon < 0) return { error: `${t} value "${v}" is not "Key: value".` };
      headers[v.slice(0, colon).trim()] = v.slice(colon + 1).trim();
      i += 2;
      continue;
    }
    if (t.startsWith("-")) {
      return { error: `Unrecognised flag "${t}".` };
    }
    positionals.push(t);
    i++;
  }

  if (positionals.length === 0) return { error: "Missing server name." };
  const rawName = positionals[0];

  if (sawSeparator) {
    if (transport === "http") {
      return { error: "--transport http does not take a command after --." };
    }
    const rest = tokens.slice(i);
    const command = rest[0];
    if (!command) return { error: "Missing command after --." };
    const args = rest.slice(1);
    const named = finalizeName(rawName, rawName);
    if (typeof named !== "string") return named;
    return { name: named, transport: "stdio", command, args, env, url: "", headers };
  }

  if (transport === "stdio") {
    return { error: "The stdio transport needs a command after --." };
  }
  if (positionals.length < 2) {
    return { error: "Missing a command (after --) or a URL." };
  }
  const url = positionals[1];
  const named = finalizeName(rawName, rawName);
  if (typeof named !== "string") return named;
  return { name: named, transport: "http", command: "", args: [], env, url, headers };
}
