import { afterEach, describe, expect, it, vi } from "vitest";
import {
  DEFAULT_MODEL_KEY,
  FLASH_MODEL_KEY,
  listModels,
  resolveModelOptions,
  settingModel,
} from "./models";
import type { SettingEntry } from "./settings";

// The models client is tested the way settings.test.ts tests its module: a
// stubbed fetch asserting the wire shape, plus the two pure fallback helpers
// the forms lean on when the request fails. There is no DOM harness and
// components are not unit-tested (TESTING.md); what needs pinning here is
// that the dropdown's option list and preselected default come from the
// harness — the endpoint and the settings registry — and that a failed fetch
// degrades to the configured defaults rather than to nothing. The server
// side of the endpoint is pinned by internal/httpapi's own models tests.

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

// modelRow builds one server settings row for a model-naming key in the
// current wire shape: the registry descriptor plus set state.
function modelRow(key: string, def: string, value?: string): SettingEntry {
  return {
    key,
    group: "Models",
    type: "string",
    default: def,
    description: "Default model",
    secret: false,
    restart: false,
    set: value !== undefined,
    override: value !== undefined && value !== def,
    ...(value !== undefined ? { value } : {}),
  };
}

describe("listModels", () => {
  it("GETs /api/models and parses the models array", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { models: ["deepseek-v4-flash", "deepseek-v4-pro", "kimi-k3"] }));

    const models = await listModels();

    expect(mock).toHaveBeenCalledWith("/api/models");
    expect(models).toEqual(["deepseek-v4-flash", "deepseek-v4-pro", "kimi-k3"]);
  });

  it("turns a server error into a readable Error carrying the message", async () => {
    stubFetch().mockResolvedValue(fakeResponse(500, { error: "internal error" }));

    await expect(listModels()).rejects.toThrow("internal error");
  });
});

describe("settingModel", () => {
  it("returns the stored override when the key is set", () => {
    expect(settingModel([modelRow(DEFAULT_MODEL_KEY, "deepseek-v4-pro", "kimi-k3")], DEFAULT_MODEL_KEY)).toBe(
      "kimi-k3",
    );
  });

  it("returns the registry default when the key is unset", () => {
    expect(settingModel([modelRow(DEFAULT_MODEL_KEY, "deepseek-v4-pro")], DEFAULT_MODEL_KEY)).toBe("deepseek-v4-pro");
  });

  it("returns empty when the settings fetch failed entirely", () => {
    expect(settingModel([], DEFAULT_MODEL_KEY)).toBe("");
  });
});

describe("resolveModelOptions", () => {
  it("prefers the endpoint's list when the fetch landed", () => {
    const entries = [modelRow(DEFAULT_MODEL_KEY, "deepseek-v4-pro"), modelRow(FLASH_MODEL_KEY, "deepseek-v4-flash")];
    expect(resolveModelOptions(["deepseek-v4-flash", "deepseek-v4-pro", "kimi-k3"], entries)).toEqual([
      "deepseek-v4-flash",
      "deepseek-v4-pro",
      "kimi-k3",
    ]);
  });

  it("falls back to the settings' two model defaults when the fetch failed, deduplicated", () => {
    const entries = [modelRow(DEFAULT_MODEL_KEY, "deepseek-v4-pro"), modelRow(FLASH_MODEL_KEY, "deepseek-v4-flash")];
    expect(resolveModelOptions(null, entries)).toEqual(["deepseek-v4-pro", "deepseek-v4-flash"]);
  });

  it("keeps an operator's overrides in the fallback list", () => {
    const entries = [
      modelRow(DEFAULT_MODEL_KEY, "deepseek-v4-pro", "kimi-k3"),
      modelRow(FLASH_MODEL_KEY, "deepseek-v4-flash"),
    ];
    expect(resolveModelOptions(null, entries)).toEqual(["kimi-k3", "deepseek-v4-flash"]);
  });

  it("deduplicates when both keys name the same model", () => {
    const entries = [
      modelRow(DEFAULT_MODEL_KEY, "deepseek-v4-pro", "kimi-k3"),
      modelRow(FLASH_MODEL_KEY, "deepseek-v4-flash", "kimi-k3"),
    ];
    expect(resolveModelOptions(null, entries)).toEqual(["kimi-k3"]);
  });

  it("is empty only when the settings fetch failed too", () => {
    expect(resolveModelOptions(null, [])).toEqual([]);
  });
});
