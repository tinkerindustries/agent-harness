import { afterEach, describe, expect, it, vi } from "vitest";
import { deleteSetting, listSettings, setSetting } from "./settings";

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

describe("listSettings", () => {
  it("GETs /api/settings and parses the entry array, omitting value for an unset key", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, [
        { key: "deepseek.api_key", set: false },
        { key: "google.api_key", set: true, value: "****abcd" },
        { key: "google.vision_model", set: true, value: "gemini-3.5-flash" },
      ]),
    );

    const entries = await listSettings();

    expect(mock).toHaveBeenCalledWith("/api/settings");
    const [entriesWithKeys] = entries;
    expect(entries).toHaveLength(3);
    expect(entries[0]).toEqual({ key: "deepseek.api_key", set: false });
    expect(entries[0].value).toBeUndefined();
    expect(entries[1]).toEqual({ key: "google.api_key", set: true, value: "****abcd" });
    expect(entries[2]).toEqual({ key: "google.vision_model", set: true, value: "gemini-3.5-flash" });
    // TS narrowing check that the unset row really has no value member.
    expect("value" in entriesWithKeys!).toBe(false);
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
    stubFetch().mockResolvedValue(fakeResponse(400, { error: 'unknown setting "deepsek.api_key"; valid settings: deepseek.api_key, google.api_key, google.vision_model' }));

    await expect(setSetting("deepsek.api_key", "sk-x")).rejects.toThrow('unknown setting "deepsek.api_key"');
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
  it("DELETEs /api/settings/{key} with the JSON content type, no body", async () => {
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
