import { afterEach, describe, expect, it, vi } from "vitest";
import {
  apiError,
  closeSession,
  closeWorkRequest,
  deleteSession,
  deleteWorkRequest,
  fetchEvents,
  fetchLastEventAt,
  formatDuration,
  getWorkRequest,
  isStuckSession,
  listLeases,
  listSessions,
  listWorkRequests,
  quietMs,
  releaseLease,
  SESSION_IDLE_THRESHOLD_MS,
  stopSession,
} from "./operations";

// The operations client is tested the way settings.test.ts tests its
// module: a stubbed fetch asserting the wire shape (method, path, content
// type, If-Match, body) and the parsing of GET responses, plus the pure
// decisions the screen is built from — which sessions count as stuck, how a
// quiet duration renders, how an error response becomes the message shown.
// There is no DOM harness and components are not unit-tested (TESTING.md);
// what needs pinning here is that every write echoes the version it was
// given and that the server's own error text is what surfaces.

// fakeResponse is a minimal stand-in for fetch's Response: enough of the
// surface (ok, status, statusText, json) for the client to do its work, no
// DOM needed.
function fakeResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "status",
    json: async () => body,
  } as Response;
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

// sessionRow builds one server session row in the current wire shape: the
// list row plus the version every row resource carries.
function sessionRow(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "sess-1",
    model: "deepseek-v4-pro",
    effort: "high",
    workspace: "/tmp/ws",
    permission_mode: "full",
    status: "running",
    created_at: "2026-01-01T00:00:00Z",
    version: 3,
    sub_turns: 0,
    usage: {
      cache_hit_tokens: 0,
      cache_miss_tokens: 0,
      completion_tokens: 0,
      reasoning_tokens: 0,
      cost_usd: 0,
    },
    ...overrides,
  };
}

function eventRow(seq: number, created_at: string): Record<string, unknown> {
  return { session_id: "sess-1", seq, kind: "content_delta", payload: {}, created_at };
}

describe("listSessions", () => {
  it("GETs /api/sessions and passes the version through untouched", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, [sessionRow(), sessionRow({ id: "sess-2", version: 1 })]));

    const sessions = await listSessions();

    expect(mock).toHaveBeenCalledWith("/api/sessions");
    expect(sessions).toHaveLength(2);
    expect(sessions[0]).toMatchObject({ id: "sess-1", version: 3, status: "running" });
    expect(sessions[1].version).toBe(1);
  });
});

describe("getWorkRequest", () => {
  it("GETs /api/requests/{request_id} and parses the row", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, {
        request_id: "req-1",
        session_id: "sess-1",
        status: "running",
        received_at: "2026-01-01T00:00:00Z",
        delivery_count: 2,
        version: 4,
      }),
    );

    const row = await getWorkRequest("req-1");

    expect(mock).toHaveBeenCalledWith("/api/requests/req-1");
    expect(row).toMatchObject({ request_id: "req-1", session_id: "sess-1", delivery_count: 2, version: 4 });
  });
});

describe("listWorkRequests", () => {
  it("GETs /api/requests and parses every row, version included — a sessionless running request included", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, [
        {
          request_id: "req-2",
          session_id: "sess-2",
          status: "running",
          received_at: "2026-01-02T00:00:00Z",
          delivery_count: 2,
          version: 3,
        },
        {
          request_id: "req-1",
          status: "running",
          received_at: "2026-01-01T00:00:00Z",
          delivery_count: 1,
          version: 1,
        },
      ]),
    );

    const rows = await listWorkRequests();

    expect(mock).toHaveBeenCalledWith("/api/requests");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({ request_id: "req-2", session_id: "sess-2", delivery_count: 2, version: 3 });
    // The sessionless row (a request whose worker died before its session
    // existed) arrives with session_id absent and its version intact — the
    // row the screen can now find without a session to walk through.
    expect(rows[1]).toMatchObject({ request_id: "req-1", version: 1 });
    expect(rows[1].session_id).toBeUndefined();
  });
});

describe("listLeases", () => {
  it("GETs /api/leases and parses the lease table", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, [
        {
          workspace: "/tmp/ws",
          session_id: "sess-1",
          acquired_at: "2026-01-01T00:00:00Z",
          heartbeat_at: "2026-01-01T01:00:00Z",
          version: 2,
        },
      ]),
    );

    const leases = await listLeases();

    expect(mock).toHaveBeenCalledWith("/api/leases");
    expect(leases[0]).toMatchObject({ workspace: "/tmp/ws", session_id: "sess-1", version: 2 });
  });
});

describe("fetchEvents", () => {
  it("GETs one page with from and limit as query parameters", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { events: [], from: 0, limit: 5000, has_more: false }));

    await fetchEvents("sess-1", 0, 5000);

    expect(mock).toHaveBeenCalledWith("/api/sessions/sess-1/events?from=0&limit=5000");
  });
});

describe("fetchLastEventAt", () => {
  it("returns the last event's created_at from a single page", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, {
        events: [eventRow(1, "2026-01-01T00:00:00Z"), eventRow(2, "2026-01-01T00:05:00Z")],
        from: 0,
        limit: 5000,
        has_more: false,
      }),
    );

    expect(await fetchLastEventAt("sess-1")).toBe("2026-01-01T00:05:00Z");
    expect(mock).toHaveBeenCalledTimes(1);
  });

  it("pages forward on has_more, echoing next as from, until the log ends", async () => {
    const mock = stubFetch();
    mock
      .mockResolvedValueOnce(
        fakeResponse(200, {
          events: [eventRow(1, "2026-01-01T00:00:00Z")],
          from: 0,
          limit: 5000,
          has_more: true,
          next: 5001,
        }),
      )
      .mockResolvedValueOnce(
        fakeResponse(200, {
          events: [eventRow(5001, "2026-01-01T00:10:00Z")],
          from: 5001,
          limit: 5000,
          has_more: false,
        }),
      );

    expect(await fetchLastEventAt("sess-1")).toBe("2026-01-01T00:10:00Z");
    expect(mock).toHaveBeenCalledTimes(2);
    expect(mock.mock.calls[1][0]).toBe("/api/sessions/sess-1/events?from=5001&limit=5000");
  });

  it("returns null for an empty log — a session with no events counts as quiet", async () => {
    stubFetch().mockResolvedValue(fakeResponse(200, { events: [], from: 0, limit: 5000, has_more: false }));

    expect(await fetchLastEventAt("sess-1")).toBeNull();
  });

  it("stops rather than looping when has_more claims more but next is absent", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, {
        events: [eventRow(1, "2026-01-01T00:00:00Z")],
        from: 0,
        limit: 5000,
        has_more: true,
      }),
    );

    expect(await fetchLastEventAt("sess-1")).toBe("2026-01-01T00:00:00Z");
    expect(mock).toHaveBeenCalledTimes(1);
  });
});

describe("closeSession", () => {
  it("PATCHes /api/sessions/{id} with the JSON content type, the If-Match version, and the status body", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, sessionRow({ status: "cancelled", version: 4 })));

    await closeSession("sess-1", 3, "cancelled");

    expect(mock).toHaveBeenCalledTimes(1);
    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/sessions/sess-1");
    expect(init).toMatchObject({
      method: "PATCH",
      headers: { "Content-Type": "application/json", "If-Match": "3" },
      body: JSON.stringify({ status: "cancelled" }),
    });
  });

  it("carries the server's 409 out as the Error — the last event is named, not replaced", async () => {
    stubFetch().mockResolvedValue(
      fakeResponse(409, { error: "store: session sess-live is still active: most recent event at 2026-01-01T10:00:00Z" }),
    );

    await expect(closeSession("sess-live", 1, "cancelled")).rejects.toThrow("most recent event at");
  });

  it("carries the server's 412 out as the Error, naming the current version", async () => {
    stubFetch().mockResolvedValue(
      fakeResponse(412, { error: "store: session sess-1: version conflict: want 3, current 4" }),
    );

    await expect(closeSession("sess-1", 3, "cancelled")).rejects.toThrow("version conflict: want 3, current 4");
  });
});

describe("deleteSession", () => {
  it("DELETEs /api/sessions/{id} with the If-Match version and no body", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { ok: true }));

    await deleteSession("sess-1", 3);

    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/sessions/sess-1");
    expect(init).toMatchObject({
      method: "DELETE",
      headers: { "Content-Type": "application/json", "If-Match": "3" },
    });
    expect((init as RequestInit).body).toBeUndefined();
  });
});

describe("closeWorkRequest", () => {
  it("PATCHes /api/requests/{request_id} with the If-Match version", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { request_id: "req-1", status: "cancelled", version: 5 }));

    await closeWorkRequest("req-1", 4, "cancelled");

    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/requests/req-1");
    expect(init).toMatchObject({
      method: "PATCH",
      headers: { "Content-Type": "application/json", "If-Match": "4" },
      body: JSON.stringify({ status: "cancelled" }),
    });
  });
});

describe("deleteWorkRequest", () => {
  it("DELETEs /api/requests/{request_id} with the If-Match version", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { ok: true }));

    await deleteWorkRequest("req-1", 4);

    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/requests/req-1");
    expect(init).toMatchObject({
      method: "DELETE",
      headers: { "Content-Type": "application/json", "If-Match": "4" },
    });
  });
});

describe("releaseLease", () => {
  it("DELETEs /api/leases/{workspace} with slashes percent-encoded and the If-Match version", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { ok: true }));

    await releaseLease("/tmp/ws", 2);

    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/leases/%2Ftmp%2Fws");
    expect(init).toMatchObject({
      method: "DELETE",
      headers: { "Content-Type": "application/json", "If-Match": "2" },
    });
  });
});

describe("isStuckSession", () => {
  const now = Date.parse("2026-01-01T01:00:00Z");

  it("counts a running session whose last event is older than the threshold", () => {
    const old = Date.parse("2026-01-01T00:00:00Z");
    expect(isStuckSession("running", old, now, SESSION_IDLE_THRESHOLD_MS)).toBe(true);
  });

  it("does not count a running session whose last event is newer than the threshold", () => {
    const recent = Date.parse("2026-01-01T00:55:00Z");
    expect(isStuckSession("running", recent, now, SESSION_IDLE_THRESHOLD_MS)).toBe(false);
  });

  it("counts a running session with no events at all — the server's idleness check lets its close through", () => {
    expect(isStuckSession("running", null, now, SESSION_IDLE_THRESHOLD_MS)).toBe(true);
  });

  it("never counts a terminal session, however quiet", () => {
    const old = Date.parse("2026-01-01T00:00:00Z");
    expect(isStuckSession("cancelled", old, now, SESSION_IDLE_THRESHOLD_MS)).toBe(false);
    expect(isStuckSession("ok", null, now, SESSION_IDLE_THRESHOLD_MS)).toBe(false);
  });
});

describe("quietMs", () => {
  const now = Date.parse("2026-01-01T01:00:00Z");

  it("measures now minus the last activity", () => {
    const last = Date.parse("2026-01-01T00:30:00Z");
    expect(quietMs(last, now)).toBe(30 * 60 * 1000);
  });

  it("clamps a timestamp in the future to zero rather than rendering a negative", () => {
    const future = now + 5000;
    expect(quietMs(future, now)).toBe(0);
  });

  it("returns null when there is no timestamp to measure from", () => {
    expect(quietMs(null, now)).toBeNull();
  });
});

describe("formatDuration", () => {
  it("renders seconds under a minute, minutes under an hour, hours and minutes beyond", () => {
    expect(formatDuration(45_000)).toBe("45s");
    expect(formatDuration(12 * 60_000)).toBe("12m");
    expect(formatDuration(3 * 3_600_000 + 5 * 60_000)).toBe("3h 5m");
  });

  it("clamps negatives and rounds down", () => {
    expect(formatDuration(-1000)).toBe("0s");
    expect(formatDuration(59_900)).toBe("59s");
  });
});

describe("stopSession", () => {
  it("POSTs /api/sessions/{id}/stop with the JSON content type, the bearer token, and the reason body, and parses the 202 acceptance", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(202, { session_id: "sess-1", stopping: true }));

    const out = await stopSession("sess-1", "tok-1", "operator intervened");

    const [url, init] = mock.mock.calls[0];
    expect(url).toBe("/api/sessions/sess-1/stop");
    expect(init).toMatchObject({
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer tok-1" },
      body: JSON.stringify({ reason: "operator intervened" }),
    });
    expect(out).toEqual({ session_id: "sess-1", stopping: true });
  });

  it("sends an empty body when there is no reason", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(202, { session_id: "sess-1", stopping: true }));

    await stopSession("sess-1", "tok-1");

    const init = mock.mock.calls[0][1] as RequestInit;
    expect(init.body).toBe("{}");
  });

  it("carries the server's 409 out as the Error — the session's actual status is named, not replaced", async () => {
    stubFetch().mockResolvedValue(
      fakeResponse(409, { error: "session sess-1 is not running in this process (status ok)" }),
    );

    await expect(stopSession("sess-1", "tok-1")).rejects.toThrow("not running in this process (status ok)");
  });
});

describe("controlToken", () => {
  // controlToken caches its result at module scope ("fetched once and
  // reused"), so each case reloads the module fresh rather than inheriting
  // the previous case's cache.
  async function freshControlToken() {
    vi.resetModules();
    const ops = await import("./operations");
    return ops.controlToken();
  }

  afterEach(() => {
    vi.resetModules();
  });

  it("GETs /api/control-token and returns the token", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { token: "tok-1" }));

    expect(await freshControlToken()).toBe("tok-1");
    expect(mock).toHaveBeenCalledWith("/api/control-token");
  });

  it("fetches once and reuses the result for the life of the module", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { token: "tok-1" }));
    vi.resetModules();
    const ops = await import("./operations");

    expect(await ops.controlToken()).toBe("tok-1");
    expect(await ops.controlToken()).toBe("tok-1");
    expect(mock).toHaveBeenCalledTimes(1);
  });

  it("treats an empty token as unavailable — a harness that never generated one, which must not get an empty bearer sent anyway", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(fakeResponse(200, { token: "" }));

    expect(await freshControlToken()).toBeNull();
    expect(mock).toHaveBeenCalledWith("/api/control-token");
  });

  it("returns null on a non-200 response, so an unconfigured or unreachable harness reads as unavailable", async () => {
    stubFetch().mockResolvedValue(fakeResponse(403, { error: "the control token is only served to loopback callers" }));

    expect(await freshControlToken()).toBeNull();
  });
});

describe("apiError", () => {
  it("carries the server's error-message body out as the Error", async () => {
    const err = await apiError(
      fakeResponse(409, { error: "store: session sess-1 is still active: most recent event at 2026-01-01T10:00:00Z" }),
    );
    expect(err.message).toContain("most recent event at");
    expect((err as Error & { status?: number }).status).toBe(409);
  });

  it("falls back to the status line when the body is not JSON", async () => {
    const res = {
      ok: false,
      status: 404,
      statusText: "Not Found",
      json: async () => {
        throw new Error("not json");
      },
    } as unknown as Response;
    const err = await apiError(res);
    expect(err.message).toBe("404 Not Found");
    expect((err as Error & { status?: number }).status).toBe(404);
  });
});
