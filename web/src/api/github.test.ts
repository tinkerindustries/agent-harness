import { afterEach, describe, expect, it, vi } from "vitest";
import { filterRepos, listGithubRepos, repoSpecFor, type GithubRepo } from "./github";

// The github client is tested the way settings.test.ts tests its module: a
// stubbed fetch asserting the wire shape (path, parsing) and the pure match
// logic in isolation. There is no DOM harness (TESTING.md); what needs
// pinning here is the /api/github/repos wire shape and the filter that turns
// typed text into suggestions.

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

// repo builds one repo row in the wire shape GET /api/github/repos returns.
function repo(fullName: string, overrides: Partial<GithubRepo> = {}): GithubRepo {
  return {
    full_name: fullName,
    clone_url: `https://github.com/${fullName}.git`,
    default_branch: "main",
    private: false,
    updated_at: "2026-01-02T03:04:05Z",
    ...overrides,
  };
}

describe("listGithubRepos", () => {
  it("GETs /api/github/repos and parses the repos and configured flag", async () => {
    const mock = stubFetch();
    mock.mockResolvedValue(
      fakeResponse(200, {
        repos: [
          repo("org/alpha"),
          repo("user/private", { default_branch: "dev", private: true }),
        ],
        configured: true,
      }),
    );

    const res = await listGithubRepos();

    expect(mock).toHaveBeenCalledWith("/api/github/repos");
    expect(res.configured).toBe(true);
    expect(res.repos).toHaveLength(2);
    expect(res.repos[0]).toEqual({
      full_name: "org/alpha",
      clone_url: "https://github.com/org/alpha.git",
      default_branch: "main",
      private: false,
      updated_at: "2026-01-02T03:04:05Z",
    });
    expect(res.repos[1].private).toBe(true);
    expect(res.repos[1].default_branch).toBe("dev");
  });

  it("passes the unconfigured state through as an empty list, not an error", async () => {
    stubFetch().mockResolvedValue(fakeResponse(200, { repos: [], configured: false }));

    const res = await listGithubRepos();

    expect(res.configured).toBe(false);
    expect(res.repos).toEqual([]);
  });

  it("turns a GitHub-side 502 into a readable Error carrying the message", async () => {
    stubFetch().mockResolvedValue(
      fakeResponse(502, { error: "GitHub rejected the token (401): Bad credentials — check github.token in Settings" }),
    );

    await expect(listGithubRepos()).rejects.toThrow("github.token");
  });
});

describe("filterRepos", () => {
  const repos = [
    repo("deepseek-harness"),
    repo("org/deepseek-tools"),
    repo("org/OTHER"),
    repo("user/website"),
  ];

  it("matches full_name substrings case-insensitively, preserving order", () => {
    expect(filterRepos(repos, "deepseek").map((r) => r.full_name)).toEqual([
      "deepseek-harness",
      "org/deepseek-tools",
    ]);
    expect(filterRepos(repos, "other").map((r) => r.full_name)).toEqual(["org/OTHER"]);
  });

  it("returns every repo, in order, for an empty or whitespace-only query, so focusing the row opens the picker", () => {
    expect(filterRepos(repos, "")).toEqual(repos);
    expect(filterRepos(repos, "   ")).toEqual(repos);
  });

  it("matches a URL the operator is typing manually nothing, keeping suggestions additive", () => {
    expect(filterRepos(repos, "https://github.com/user/website.git#main")).toEqual([]);
  });

  it("matches nothing when the list is empty or the query misses", () => {
    expect(filterRepos([], "deepseek")).toEqual([]);
    expect(filterRepos(repos, "nope")).toEqual([]);
  });
});

describe("repoSpecFor", () => {
  it("fills a repo row with the URL#branch spec the form already parses", () => {
    expect(repoSpecFor(repo("org/alpha"))).toBe("https://github.com/org/alpha.git#main");
    expect(repoSpecFor(repo("user/private", { default_branch: "dev" }))).toBe(
      "https://github.com/user/private.git#dev",
    );
  });
});
