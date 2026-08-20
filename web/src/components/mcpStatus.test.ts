import { describe, expect, it } from "vitest";
import { buildKVPatch, formatArgs, formatCommandLine, probeStatus, relativeTime, targetLine } from "./mcpStatus";

describe("formatCommandLine", () => {
  it("joins a command and its args with spaces", () => {
    expect(formatCommandLine("uvx", ["blender-mcp"])).toBe("uvx blender-mcp");
  });

  it("renders a bare command with no args", () => {
    expect(formatCommandLine("uvx", [])).toBe("uvx");
  });

  it("quotes an argument that contains whitespace", () => {
    expect(formatCommandLine("node", ["server.js", "--name", "my server"])).toBe(
      'node server.js --name "my server"',
    );
  });

  it("escapes a double quote inside a quoted argument", () => {
    expect(formatCommandLine("node", ['say "hi" there'])).toBe('node "say \\"hi\\" there"');
  });

  it("quotes an empty-string argument so it round-trips as a distinct token", () => {
    expect(formatCommandLine("node", ["", "x"])).toBe('node "" x');
  });
});

describe("formatArgs", () => {
  it("renders an empty list as an empty string", () => {
    expect(formatArgs([])).toBe("");
  });

  it("quotes only the args that need it", () => {
    expect(formatArgs(["-y", "@modelcontextprotocol/server-filesystem", "/tmp"])).toBe(
      "-y @modelcontextprotocol/server-filesystem /tmp",
    );
  });
});

describe("targetLine", () => {
  it("renders the command line for a stdio server", () => {
    expect(targetLine({ transport: "stdio", command: "uvx", args: ["blender-mcp"], url: "" })).toBe(
      "uvx blender-mcp",
    );
  });

  it("renders the URL for an http server, ignoring command/args", () => {
    expect(
      targetLine({ transport: "http", command: "", args: [], url: "https://example.com/mcp" }),
    ).toBe("https://example.com/mcp");
  });
});

describe("relativeTime", () => {
  const now = Date.parse("2026-08-21T12:00:00Z");

  it("renders an empty timestamp as never", () => {
    expect(relativeTime("", now)).toBe("never");
  });

  it("renders an unparseable timestamp as itself", () => {
    expect(relativeTime("not-a-date", now)).toBe("not-a-date");
  });

  it("renders a moment just now", () => {
    expect(relativeTime(new Date(now - 2000).toISOString(), now)).toBe("just now");
  });

  it("renders seconds under a minute", () => {
    expect(relativeTime(new Date(now - 45_000).toISOString(), now)).toBe("45s ago");
  });

  it("renders minutes under an hour, singular for one", () => {
    expect(relativeTime(new Date(now - 4 * 60_000).toISOString(), now)).toBe("4 minutes ago");
    expect(relativeTime(new Date(now - 60_000).toISOString(), now)).toBe("1 minute ago");
  });

  it("renders hours under a day, singular for one", () => {
    expect(relativeTime(new Date(now - 3 * 3600_000).toISOString(), now)).toBe("3 hours ago");
    expect(relativeTime(new Date(now - 3600_000).toISOString(), now)).toBe("1 hour ago");
  });

  it("renders days beyond that, singular for one", () => {
    expect(relativeTime(new Date(now - 2 * 86_400_000).toISOString(), now)).toBe("2 days ago");
    expect(relativeTime(new Date(now - 86_400_000).toISOString(), now)).toBe("1 day ago");
  });

  it("clamps a future timestamp to just now rather than a negative duration", () => {
    expect(relativeTime(new Date(now + 5000).toISOString(), now)).toBe("just now");
  });
});

describe("probeStatus", () => {
  const now = Date.parse("2026-08-21T12:00:00Z");

  it("reports an error state — and its message — when probe_error is set, even if probed_at names an earlier success", () => {
    const status = probeStatus(
      { probe_error: "exec: \"uvx\": executable file not found in $PATH", probed_at: "2026-08-21T11:00:00Z", tool_count: 7 },
      now,
    );
    expect(status.label).toBe("ERROR");
    expect(status.variant).toBe("failed");
    expect(status.detail).toBe('exec: "uvx": executable file not found in $PATH');
  });

  it("reports never-probed when probe_error is empty and probed_at is empty", () => {
    const status = probeStatus({ probe_error: "", probed_at: "", tool_count: 0 }, now);
    expect(status.label).toBe("NEVER PROBED");
    expect(status.variant).toBe("outline");
    expect(status.detail).toMatch(/never probed/i);
  });

  it("reports ok with a tool count and relative probe time when both signals are healthy", () => {
    const status = probeStatus(
      { probe_error: "", probed_at: new Date(now - 4 * 60_000).toISOString(), tool_count: 7 },
      now,
    );
    expect(status.label).toBe("OK");
    expect(status.variant).toBe("done");
    expect(status.detail).toBe("7 tools · probed 4 minutes ago");
  });

  it("singularizes 'tool' for a count of one", () => {
    const status = probeStatus({ probe_error: "", probed_at: new Date(now).toISOString(), tool_count: 1 }, now);
    expect(status.detail).toMatch(/^1 tool ·/);
  });

  it("reports zero tools without pluralization breaking", () => {
    const status = probeStatus({ probe_error: "", probed_at: new Date(now).toISOString(), tool_count: 0 }, now);
    expect(status.detail).toMatch(/^0 tools ·/);
  });
});

describe("buildKVPatch", () => {
  it("sends a create's rows verbatim, even one seeded with touched: false", () => {
    const rows = [
      { key: "API_KEY", value: "sk-abc123", touched: true },
      { key: "REGION", value: "us-east", touched: false },
    ];
    expect(buildKVPatch(rows, "add")).toEqual({ API_KEY: "sk-abc123", REGION: "us-east" });
  });

  it("sends the empty string for an edit-mode row that was never touched, keeping the stored secret", () => {
    const rows = [{ key: "API_KEY", value: "****cdef", touched: false }];
    expect(buildKVPatch(rows, "edit")).toEqual({ API_KEY: "" });
  });

  it("sends a touched row's real value on an edit", () => {
    const rows = [{ key: "API_KEY", value: "sk-newvalue", touched: true }];
    expect(buildKVPatch(rows, "edit")).toEqual({ API_KEY: "sk-newvalue" });
  });

  it("omits a row deleted from the list entirely — the map simply doesn't carry it", () => {
    // Simulated by the caller never including the row; buildKVPatch only
    // ever sees what is still in the array.
    const rows = [{ key: "KEEP", value: "x", touched: true }];
    expect(buildKVPatch(rows, "edit")).toEqual({ KEEP: "x" });
  });

  it("drops a row with a blank key", () => {
    const rows = [
      { key: "", value: "orphaned", touched: true },
      { key: "REAL", value: "v", touched: true },
    ];
    expect(buildKVPatch(rows, "add")).toEqual({ REAL: "v" });
  });

  it("trims whitespace from the key", () => {
    const rows = [{ key: "  SPACED  ", value: "v", touched: true }];
    expect(buildKVPatch(rows, "add")).toEqual({ SPACED: "v" });
  });

  it("renders an empty row list as an empty map", () => {
    expect(buildKVPatch([], "edit")).toEqual({});
  });
});
