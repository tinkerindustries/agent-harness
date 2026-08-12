import { afterEach, describe, expect, it, vi } from "vitest";
import { deleteSetting, listSettings, secretMask, setSetting } from "./settings";

// The settings client is tested the way fold.test.ts tests its module: a
// stubbed fetch asserting the wire shape (method, path, content type, body)
// and the parsing of a GET response. There is no DOM harness and components
// are not unit-tested (TESTING.md); what needs pinning here is that a write
// carries the headers the server demands and that an error response becomes
// a readable error rather than a silent success. The server side of these
// endpoints is pinned by internal/httpapi's own settings tests.

// fakeResponse is a minimal stand-in for fetch's Response: enough of the
// surface (ok, status, json) for the client to do its work, no DOM needed.
function fakeResponse(status: number, body: unknown): Response {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as Response;
}

let fetchMock: ReturnType<typeof vi.fn>;

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubFetch(): ReturnType<typeof vi.fn> {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

// registryRow builds one server row in the current wire shape: the registry
// descriptor plus set state. The client must pass it through untouched.
function registryRow(overrides: Partial<Parameters<typeof Object.assign>[0]> = {}): Record<string, unknown> {
  return {
    key: "run.max_tokens",
    group: "Run budget",
    type: "integer",
    default: "48000",
    description: "Default max output tokens",
    secret: false,
    restart: false,
    set: false,
    override: false,
    ...overrides,
  };
}

describe("listSettings", () => {
  it("GETs /api/settings and parses the descriptor rows, omitting value for an unset key", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, [
        registryRow(),
        registryRow({
          key: "deepseek.api_key",
          group: "Credentials",
          type: "string",
          default: "",
          secret: true,
          set: true,
          override: true,
          value: "****abcd",
        }),
        registryRow({ key: "worker.pool_size", group: "Requires a restart", default: "4", restart: true }),
      ]),
    );

    const entries = await listSettings();

    expect(mock).toHaveBeenCalledWith("/api/settings");
    expect(entries).toHaveLength(3);
    expect(entries[0]).toEqual({
      key: "run.max_tokens",
      group: "Run budget",
      type: "integer",
      default: "48000",
      description: "Default max output tokens",
      secret: false,
      restart: false,
      set: false,
      override: false,
    });
    expect(entries[0].value).toBeUndefined();
    expect(entries[1].value).toBe("****abcd");
    expect(entries[1].secret).toBe(true);
    expect(entries[2].restart).toBe(true);
  });

  it("passes the phase-7 bounds through untouched: numbers for integers, Go duration text for durations, closed sets for allowed", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, [
        registryRow(), // run.max_tokens, integer — no min/max in this row yet
        registryRow({
          key: "run.max_tokens",
          group: "Run budget",
          type: "integer",
          min: 1,
          max: 1000000,
        }),
        registryRow({
          key: "tools.bash_timeout",
          group: "Tool limits",
          type: "duration",
          default: "2m",
          min: "1s",
          max: "24h",
        }),
        registryRow({
          key: "model.effort",
          group: "Models",
          type: "string",
          default: "high",
          allowed: ["low", "high", "max"],
        }),
        registryRow({
          key: "model.default",
          group: "Models",
          type: "string",
          default: "deepseek-v4-pro",
        }),
      ]),
    );

    const entries = await listSettings();

    expect(entries[0].min).toBeUndefined();
    expect(entries[1]).toMatchObject({ min: 1, max: 1000000 });
    expect(entries[2]).toMatchObject({ min: "1s", max: "24h" });
    expect(entries[3].allowed).toEqual(["low", "high", "max"]);
    // A plain string setting arrives without the pair of meaningless zeroes.
    expect(entries[4].min).toBeUndefined();
    expect(entries[4].max).toBeUndefined();
    expect(entries[4].allowed).toBeUndefined();
  });

  it("turns a server error into a readable Error carrying the message", async () => {
    stubFetch().mockResolvedValue(fakeResponse(500, { error: "internal error" }));

    await expect(listSettings()).rejects.toThrow("internal error");
  });
});

describe("setSetting", () => {
  it('PUTs /api/settings/{key} with the JSON content type and a {"value": "..."} body', async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { ok: true }));

    await setSetting("deepseek.api_key", "sk-abc123");

    expect(mock).toHaveBeenCalledTimes(1);
    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/settings/deepseek.api_key");
    expect(init).toMatchObject({
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ value: "sk-abc123" }),
    });
  });

  it("carries a 400 unknown-key message out as the Error", async () => {
    stubFetch().mockResolvedValue(fakeResponse(400, { error: 'unknown setting "deepsek.api_key"; valid settings: ...' }));

    await expect(setSetting("deepsek.api_key", "sk-x")).rejects.toThrow('unknown setting "deepsek.api_key"');
  });

  it("carries a 400 validation message out as the Error — the registry's bound, surfaced verbatim", async () => {
    stubFetch().mockResolvedValue(fakeResponse(400, { error: "tools.bash_timeout: -5s is out of range [1s, 24h0m0s]" }));

    await expect(setSetting("tools.bash_timeout", "-5s")).rejects.toThrow("out of range");
  });

  it("falls back to the status line when the error body is not JSON", async () => {
    const res = {
      ok: false,
      status: 415,
      statusText: "Unsupported Media Type",
      json: async () => {
        throw new Error("not json");
      },
    } as unknown as Response;
    stubFetch().mockResolvedValue(res);

    await expect(setSetting("google.api_key", "x")).rejects.toThrow("415 Unsupported Media Type");
  });
});

describe("deleteSetting", () => {
  it("DELETEs /api/settings/{key} with the JSON content type, no body — the screen's 'reset to default'", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { ok: true }));

    await deleteSetting("google.vision_model");

    expect(mock).toHaveBeenCalledTimes(1);
    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/settings/google.vision_model");
    expect(init).toMatchObject({
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
    });
    expect((init as RequestInit).body).toBeUndefined();
  });

  it("turns a 403 into a readable Error rather than a silent success", async () => {
    stubFetch().mockResolvedValue(fakeResponse(403, { error: "cross-origin write refused" }));

    await expect(deleteSetting("deepseek.api_key")).rejects.toThrow("cross-origin write refused");
  });
});

// secretMask is the screen's fixed-width rendering of the server's mask: the
// wire value grows with the secret (****abcd for 8 characters, **********abcd
// for 12), so the row's mask width would report how long the secret is. The
// fixed rendering must not.
describe("secretMask", () => {
  it("keeps the last four revealed characters and a fixed four-asterisk head", () => {
    expect(secretMask("****abcd")).toBe("****abcd");
    expect(secretMask("**********abcd")).toBe("****abcd");
    expect(secretMask("**********abcdefgh")).toBe("****efgh");
  });

  it("reveals nothing for a secret of four characters or fewer and still renders at full width", () => {
    // The server's mask for a short secret is asterisks only; the fixed
    // rendering pads to the same eight characters without revealing a thing.
    expect(secretMask("****")).toBe("********");
    expect(secretMask("***")).toBe("********");
    expect(secretMask("**")).toBe("********");
    expect(secretMask("*")).toBe("********");
  });

  it("renders an empty mask at the same fixed width", () => {
    expect(secretMask("")).toBe("********");
  });
});
