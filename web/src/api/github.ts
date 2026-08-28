// The GitHub repos client: the fetch call and wire types for
// GET /api/github/repos (docs/DATA-API.md "github repos"), plus the pure
// match logic the start-run form's repo picker is built from. Kept as a
// module separate from the React component, like settings.ts and operations.ts
// beside it — the component renders, this talks to the server and filters,
// and the tests pin this module's wire shape and matching without any DOM.
//
// The endpoint is deliberately quiet when it cannot help: no GitHub
// credential in Settings — neither a GitHub App nor github.token — means
// {"repos": [], "configured": false}, the expected unconfigured state, and a
// GitHub-side failure is a 502 whose message this module carries out via
// apiError, so the form shows a hint either way instead of blocking.

import { apiError } from "./operations";

// GithubRepo is one row of GET /api/github/repos's repos array, mirroring
// internal/httpapi.githubRepo: the full name the picker searches, the clone
// URL and default branch the repo row is filled with, whether it is private,
// and when it was last updated. The server preserves GitHub's
// newest-updated-first order, so the picker lists repos in that order.
export interface GithubRepo {
  full_name: string;
  clone_url: string;
  default_branch: string;
  private: boolean;
  updated_at: string;
}

// GithubReposResponse is GET /api/github/repos's 200 body. configured tells
// the form "no token configured yet" apart from "token set but the GitHub
// call failed": false with an empty repos array is the expected unconfigured
// state, not an error.
export interface GithubReposResponse {
  repos: GithubRepo[];
  configured: boolean;
}

// listGithubRepos fetches GET /api/github/repos: the operator's GitHub
// repositories, newest-updated first — every installation's merged into one
// list when a GitHub App is configured, one account's under github.token
// (docs/GITHUB-APP.md) — or the unconfigured empty response when neither is
// set. A non-2xx answer throws the server's error message
// (a 502 names the GitHub side of the failure), which the form surfaces as a
// quiet hint rather than blocking the start form.
export async function listGithubRepos(): Promise<GithubReposResponse> {
  const res = await fetch("/api/github/repos");
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as GithubReposResponse;
}

// filterRepos returns the repos whose full_name contains query,
// case-insensitively, in the server's updated-at order. An empty or
// whitespace-only query returns every repo — focusing the row is what opens
// the list — while a non-empty query narrows by substring, so a git URL typed
// by hand still matches nothing and the suggestions stay additive.
export function filterRepos(repos: GithubRepo[], query: string): GithubRepo[] {
  const q = query.trim().toLowerCase();
  if (q === "") return repos;
  return repos.filter((r) => r.full_name.toLowerCase().includes(q));
}

// repoSpecFor fills a repo row's input with the value the form already
// parses, URL#branch (operations.ts parseRepoSpec): the clone URL plus the
// default branch the token can see. The picker deliberately stops at the
// default branch — a branch picker is out of scope for this feature.
export function repoSpecFor(repo: GithubRepo): string {
  return `${repo.clone_url}#${repo.default_branch}`;
}
