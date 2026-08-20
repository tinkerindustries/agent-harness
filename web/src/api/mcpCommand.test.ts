import { describe, expect, it } from "vitest";
import { parseMCPCommand, sanitizeName, splitCommandLine, type ParsedMCPServer } from "./mcpCommand";

// parseMCPCommand is pure logic with no fetch and no DOM (mcpCommand.ts),
// so it is tested the way fold.test.ts and operations.test.ts test their
// modules: plain inputs and outputs, no stub, no store.

function ok(result: ReturnType<typeof parseMCPCommand>): ParsedMCPServer {
  if ("error" in result) throw new Error(`expected success, got error: ${result.error}`);
  return result;
}

function err(result: ReturnType<typeof parseMCPCommand>): string {
  if (!("error" in result)) throw new Error(`expected an error, got a parsed server: ${JSON.stringify(result)}`);
  return result.error;
}

describe("parseMCPCommand — claude mcp add, stdio", () => {
  it("parses the exact motivating command", () => {
    const parsed = ok(parseMCPCommand("claude mcp add --scope user blender -- uvx blender-mcp"));
    expect(parsed).toEqual({
      name: "blender",
      transport: "stdio",
      command: "uvx",
      args: ["blender-mcp"],
      env: {},
      url: "",
      headers: {},
    });
  });

  it("ignores --scope regardless of its value — this harness's config is global", () => {
    const project = ok(parseMCPCommand("claude mcp add --scope project blender -- uvx blender-mcp"));
    const local = ok(parseMCPCommand("claude mcp add --scope local blender -- uvx blender-mcp"));
    expect(project.name).toBe("blender");
    expect(local.name).toBe("blender");
  });

  it("works with no --scope flag at all", () => {
    const parsed = ok(parseMCPCommand("claude mcp add blender -- uvx blender-mcp"));
    expect(parsed).toMatchObject({ name: "blender", command: "uvx", args: ["blender-mcp"] });
  });

  it("collects repeated -e flags into env", () => {
    const parsed = ok(
      parseMCPCommand("claude mcp add myserver -e API_KEY=abc123 -e REGION=us-east -- node server.js"),
    );
    expect(parsed.env).toEqual({ API_KEY: "abc123", REGION: "us-east" });
    expect(parsed.command).toBe("node");
    expect(parsed.args).toEqual(["server.js"]);
  });

  it("accepts the long form --env identically to -e", () => {
    const parsed = ok(parseMCPCommand("claude mcp add myserver --env API_KEY=abc123 -- node server.js"));
    expect(parsed.env).toEqual({ API_KEY: "abc123" });
  });

  it("handles a quoted -e value containing a space", () => {
    const parsed = ok(parseMCPCommand('claude mcp add myserver -e "API_KEY=a value" -- node server.js'));
    expect(parsed.env).toEqual({ API_KEY: "a value" });
  });

  it("splits KEY=value only on the first '=' so a value with '=' survives", () => {
    const parsed = ok(parseMCPCommand("claude mcp add myserver -e TOKEN=a=b=c -- node server.js"));
    expect(parsed.env).toEqual({ TOKEN: "a=b=c" });
  });

  it("passes multiple trailing args through to the stdio command", () => {
    const parsed = ok(parseMCPCommand("claude mcp add fs -- npx -y @modelcontextprotocol/server-filesystem /tmp"));
    expect(parsed).toMatchObject({
      name: "fs",
      transport: "stdio",
      command: "npx",
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
    });
  });
});

describe("parseMCPCommand — claude mcp add, http", () => {
  it("parses --transport http with a trailing URL", () => {
    const parsed = ok(parseMCPCommand("claude mcp add --transport http myserver https://example.com/mcp"));
    expect(parsed).toEqual({
      name: "myserver",
      transport: "http",
      command: "",
      args: [],
      env: {},
      url: "https://example.com/mcp",
      headers: {},
    });
  });

  it("collects repeated -H headers, quoted 'Key: value' pairs", () => {
    const parsed = ok(
      parseMCPCommand(
        'claude mcp add --transport http myserver -H "Authorization: Bearer xyz" -H "X-Team: infra" https://example.com/mcp',
      ),
    );
    expect(parsed.headers).toEqual({ Authorization: "Bearer xyz", "X-Team": "infra" });
  });

  it("accepts the long form --header identically to -H", () => {
    const parsed = ok(
      parseMCPCommand('claude mcp add --transport http myserver --header "Authorization: Bearer xyz" https://x/mcp'),
    );
    expect(parsed.headers).toEqual({ Authorization: "Bearer xyz" });
  });

  it("rejects --transport sse, naming the unsupported transport", () => {
    const message = err(parseMCPCommand("claude mcp add --transport sse myserver https://example.com/mcp"));
    expect(message).toMatch(/sse/i);
    expect(message).toMatch(/stdio and http/i);
  });

  it("rejects an unknown transport value", () => {
    expect(err(parseMCPCommand("claude mcp add --transport carrier-pigeon myserver https://x/mcp"))).toMatch(
      /unknown transport/i,
    );
  });

  it("rejects --transport http paired with a piped command", () => {
    expect(err(parseMCPCommand("claude mcp add --transport http myserver -- uvx blender-mcp"))).toMatch(
      /does not take a command/i,
    );
  });

  it("rejects --transport stdio with no command after --", () => {
    expect(err(parseMCPCommand("claude mcp add --transport stdio myserver https://example.com/mcp"))).toMatch(
      /needs a command/i,
    );
  });
});

describe("parseMCPCommand — bare stdio commands", () => {
  it("parses a bare runner and package with no flags", () => {
    const parsed = ok(parseMCPCommand("uvx blender-mcp"));
    expect(parsed).toEqual({
      name: "blender-mcp",
      transport: "stdio",
      command: "uvx",
      args: ["blender-mcp"],
      env: {},
      url: "",
      headers: {},
    });
  });

  it("infers a name from a scoped npm package, stripping the scope and a server- prefix", () => {
    const parsed = ok(parseMCPCommand("npx -y @modelcontextprotocol/server-filesystem /tmp"));
    expect(parsed).toEqual({
      name: "filesystem",
      transport: "stdio",
      command: "npx",
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      env: {},
      url: "",
      headers: {},
    });
  });

  it("skips a leading runner flag (npx -y) when hunting for the name argument", () => {
    const parsed = ok(parseMCPCommand("npx -y some-random-tool"));
    expect(parsed.name).toBe("some-random-tool");
  });

  it("falls back to the runner itself when every argument is a flag", () => {
    const parsed = ok(parseMCPCommand("my-server --verbose"));
    expect(parsed.name).toBe("my-server");
    expect(parsed.args).toEqual(["--verbose"]);
  });
});

describe("parseMCPCommand — bare URLs", () => {
  it("parses a bare URL as an http server named after its host", () => {
    const parsed = ok(parseMCPCommand("https://example.com/mcp"));
    expect(parsed).toEqual({
      name: "example",
      transport: "http",
      command: "",
      args: [],
      env: {},
      url: "https://example.com/mcp",
      headers: {},
    });
  });

  it("handles a bare http:// URL too", () => {
    const parsed = ok(parseMCPCommand("http://localhost:8080/mcp"));
    expect(parsed.transport).toBe("http");
    expect(parsed.url).toBe("http://localhost:8080/mcp");
  });
});

describe("parseMCPCommand — quoting", () => {
  it("accepts single-quoted arguments", () => {
    const parsed = ok(parseMCPCommand("claude mcp add myserver -e 'API_KEY=a value' -- node server.js"));
    expect(parsed.env).toEqual({ API_KEY: "a value" });
  });

  it("errors on an unterminated quote", () => {
    expect(err(parseMCPCommand('claude mcp add myserver -e "API_KEY=unterminated -- node server.js'))).toMatch(
      /quote/i,
    );
  });
});

describe("parseMCPCommand — errors", () => {
  it("rejects an empty paste", () => {
    expect(err(parseMCPCommand(""))).toMatch(/paste a command/i);
    expect(err(parseMCPCommand("   "))).toMatch(/paste a command/i);
  });

  it("rejects claude mcp add with no server name", () => {
    expect(err(parseMCPCommand("claude mcp add -- uvx blender-mcp"))).toMatch(/missing server name/i);
  });

  it("rejects claude mcp add with a name but neither a command nor a URL", () => {
    expect(err(parseMCPCommand("claude mcp add blender"))).toMatch(/missing a command|url/i);
  });

  it("rejects a -- with nothing after it", () => {
    expect(err(parseMCPCommand("claude mcp add blender --"))).toMatch(/missing command/i);
  });

  it("rejects an unrecognised flag", () => {
    expect(err(parseMCPCommand("claude mcp add --bogus-flag blender -- uvx blender-mcp"))).toMatch(/unrecognised flag/i);
  });

  it("rejects -e with no argument at all", () => {
    expect(err(parseMCPCommand("claude mcp add myserver -e"))).toMatch(/needs a key=value/i);
  });

  it("rejects -e whose value has no '='", () => {
    expect(err(parseMCPCommand("claude mcp add myserver -e NOTKEYVALUE -- node server.js"))).toMatch(/is not key=value/i);
  });

  it("rejects -H whose value has no ':'", () => {
    expect(
      err(parseMCPCommand('claude mcp add --transport http myserver -H "NoColonHere" https://x/mcp')),
    ).toMatch(/is not "key: value"/i);
  });

  it("rejects a name that sanitizes to nothing", () => {
    expect(err(parseMCPCommand("claude mcp add ___ -- uvx blender-mcp"))).toMatch(/could not turn/i);
  });

  it("rejects an unparseable bare URL-looking token gracefully via the bare-command path", () => {
    // Not recognised as a bare URL (it has a second token), so it is parsed
    // as a stdio command instead — a legitimate outcome, not an error.
    const parsed = ok(parseMCPCommand("https://example.com/mcp --flag"));
    expect(parsed.transport).toBe("stdio");
    expect(parsed.command).toBe("https://example.com/mcp");
  });
});

describe("splitCommandLine", () => {
  it("splits plain whitespace-separated words", () => {
    expect(splitCommandLine("-y @modelcontextprotocol/server-filesystem /tmp")).toEqual([
      "-y",
      "@modelcontextprotocol/server-filesystem",
      "/tmp",
    ]);
  });

  it("keeps a quoted argument as one token", () => {
    expect(splitCommandLine('--name "my server"')).toEqual(["--name", "my server"]);
  });

  it("returns an empty array for blank text", () => {
    expect(splitCommandLine("   ")).toEqual([]);
  });

  it("returns null for an unterminated quote", () => {
    expect(splitCommandLine('--name "unterminated')).toBeNull();
  });
});

describe("sanitizeName", () => {
  it("lowercases and passes through an already-valid name", () => {
    expect(sanitizeName("Blender")).toBe("blender");
  });

  it("strips an npm scope", () => {
    expect(sanitizeName("@modelcontextprotocol/server-filesystem")).toBe("filesystem");
  });

  it("strips a server- prefix with no scope", () => {
    expect(sanitizeName("server-filesystem")).toBe("filesystem");
  });

  it("replaces illegal characters with '-'", () => {
    expect(sanitizeName("my cool server!")).toBe("my-cool-server-");
  });

  it("drops a leading run of characters that cannot start the name", () => {
    expect(sanitizeName("---blender")).toBe("blender");
  });

  it("truncates to 32 characters", () => {
    const long = "a".repeat(50);
    expect(sanitizeName(long)).toHaveLength(32);
    expect(sanitizeName(long)).toBe("a".repeat(32));
  });

  it("collapses to empty for input with nothing usable", () => {
    expect(sanitizeName("!!!")).toBe("");
    expect(sanitizeName("")).toBe("");
  });
});
