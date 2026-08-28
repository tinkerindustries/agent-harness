# The GitHub App

The harness can authenticate to GitHub as a **GitHub App** instead of as one
account's personal access token. The reason to: a fine-grained personal
access token belongs to exactly one account, so a harness working across a
personal account and an organisation had to pick one. An App is installed
separately on each account, and one App id plus one private key then reaches
both.

Nothing here is required. With no App configured the harness behaves exactly
as it did before — `github.token`, one account — and that path is unchanged.

## Setting one up

1. **Create the App.** GitHub → *Settings* → *Developer settings* → *GitHub
   Apps* → *New GitHub App*, or the same path under the organisation's
   settings to have the organisation own it.
   - Homepage URL: anything (nothing calls it).
   - Uncheck **Active** under Webhook. The harness listens for nothing.
   - **Set "Where can this GitHub App be installed?" to "Any account".**
     This is the setting that decides whether one App can cover two
     accounts, and it is easy to miss because the default is the other one.
     A private App — "Only on this account" — can be installed *only on the
     account that owns it*, so a private App owned by your personal account
     cannot be installed on an organisation even if you own that
     organisation too, and vice versa. Reach is decided here, not by who
     owns the App.

   Ownership decides who administers it and who can rotate its private key:
   an organisation-owned App is managed by that organisation's owners and
   outlives any one person's account, a personal one is yours alone. Pick on
   that basis; both reach the same accounts once the App is public.

   "Any account" means anyone holding the App's URL can install it on their
   own account. That gives them nothing of yours — using an App requires its
   private key, which stays here — but it does mean their repositories would
   join this harness's picker, because the picker lists every installation.
   Nobody is likely to find the URL, and the consequence if they do is
   clutter.
2. **Permissions.** Under *Repository permissions*:
   - **Contents: Read and write** — clone, fetch, push.
   - **Pull requests: Read and write** — `gh pr create`.
   - **Metadata: Read-only** — mandatory, GitHub adds it for you.
   - Add **Issues**, **Actions**, or **Workflows** only if the agents you run
     need them. Workflows in particular is what a run needs to push a change
     under `.github/workflows/`; without it the push is refused.
3. **Create it**, then on the App's own settings page note the **App ID**
   (a number) and press **Generate a private key**. The browser downloads a
   `.pem`.
4. **Install it** — *Install App* in the left-hand nav — once on your
   personal account and once on the organisation, choosing either all
   repositories or a selected set each time. An organisation you do not own
   will ask an owner to approve the installation. Only one account is
   offered here if the App is still private; go back to step 1's visibility
   setting.
5. **Configure the harness**, on the Settings screen:
   - `github.app_id` — the number from step 3.
   - `github.app_private_key` — the contents of the `.pem`, pasted whole.
     The field is a single line and the PEM is not; paste it anyway. The
     parser strips whitespace and accepts the flattened paste, the canonical
     PEM, and a bare base64 key alike
     (`internal/githubapp.ParsePrivateKey`).

     If your browser truncates the paste at the first newline, flatten the
     file first and paste that:

     ```sh
     tr -d '\n' < ~/Downloads/your-app.private-key.pem | pbcopy
     ```

     Or skip the screen and put the file straight to the API:

     ```sh
     curl -X PUT localhost:8080/api/settings/github.app_private_key \
       -H 'Content-Type: application/json' \
       -d "$(jq -Rs '{value: .}' ~/Downloads/your-app.private-key.pem)"
     ```

Both take effect immediately — no restart. The moment both are set, the App
takes over from `github.token`, which can be left in place as a fallback or
cleared.

To check it, open the start-run form's repository picker: it lists every
installation's repositories merged into one list. An empty list, or a hint
naming a GitHub error, means the credential is wrong or the App is not
installed where you think.

## How it works

An App has no standing credential. It signs a JWT with its private key and
exchanges that for an **installation token** — one per account it is
installed on, each good for an hour, each usable only on that account's
repositories. So there is no single string to write into a credential file,
and every consumer has to say which account it wants.

`internal/githubapp` holds that: the JWT, the account-login-to-installation
map, the token minting, and the cache in front of both. `internal/githubauth`
decides which credential is live — the App when both settings are set and a
helper command is available, the personal access token otherwise — and makes
it ambient through the process environment, which every subprocess a session
spawns inherits.

### How git gets a token

git is pointed at a credential helper rather than a credential file:

```
credential.helper = !<path to harness> github-credential
credential.useHttpPath = true
```

both published through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/
`GIT_CONFIG_VALUE_n` in the harness process's environment, so nothing is
written to the host's global git config.

When git needs a credential it runs `harness github-credential`, which reads
the request on stdin, takes the owner from the repository path (that is what
`useHttpPath` is for — without it git sends no path and there is no owner to
route on), and asks the running harness for that account's token over
`POST /api/github/credential`. The helper talks HTTP to the harness rather
than minting its own token because the harness is the process holding the
private key and the token cache; a helper minting for itself would be a
round trip to GitHub on every push.

That endpoint has its own bearer token, generated at startup and passed to
the helper through the environment. It is deliberately not the run-control
token: every session can read the environment it runs in, and minting
installation tokens is something a session's own git can do anyway, whereas
stopping other people's runs is not.

Every git operation in a session — the clone the workspace makes, a fetch, a
push, a `git ls-remote` against some third repository — goes through this and
gets the right account's token, refreshed as it expires. None of it needs the
session to know anything.

### What gh gets

`gh` reads a single `GH_TOKEN` and has no notion of a token per account, so
it cannot be routed the way git is. Instead each `Bash` call is given a
`GH_TOKEN` resolved at the moment of the call: the harness reads the remotes
of the clones in that session's workspace, takes the first account it can
mint a token for, and puts it in the command's environment
(`cmd/harness/github.go`, `tools.Executor.ExtraEnv`). Because it is resolved
per call, a run longer than an hour never carries a stale token.

The limit worth knowing: **a session holding clones from two different
accounts gets one of them for `gh`**, and `gh` commands against the other
will be refused. git is unaffected — pushing, pulling and branching stay
correct for every repository in the workspace — so this only bites a run
using `gh` against a repository other than the one it is working in. Running
such work as two runs, one per account, avoids it entirely.

## What changes about the runs

Commits keep whatever author the session's git config gives them, but the
**push and the pull request come from the App's bot account**
(`<app-name>[bot]`), not from you. That is usually what you want from an
agent harness — the actions are attributable to the harness rather than to a
person — but it is worth knowing before turning it on:

- A pull request opened by the App counts as a bot PR. Some `on:
  pull_request` workflows and some branch protection rules treat those
  differently.
- The App must be installed on a repository before the harness can touch it.
  A repository added to an organisation later needs adding to the
  installation, unless the installation is "all repositories".
- Anything the App is not granted, it cannot do — including pushing changes
  under `.github/workflows/` without the Workflows permission.

## When it fails

Every failure surfaces where the operation was: a failed clone or push
carries the helper's message, the settings screen and the repo picker carry
the endpoint's.

- *"the GitHub App is not installed on `<account>` (it is installed on
  ...)"* — install it on that account, or the run is working on a repository
  it was never given.
- *"GitHub rejected the App credential (401)"* — the App id and the private
  key disagree, or the key has been revoked. Regenerate the key and paste it
  again.
- *"`github.app_id` is `Iv1....`, which is not the numeric App id"* — that is
  the App's *client* id. The App id is the number above it on the same page.
- The repo picker is empty with both settings filled in — the App is
  installed nowhere. Install it on at least one account.
- The picker shows one account when you installed on two — the second
  installation did not happen, which is what a private App produces: it can
  only be installed on the account that owns it. Set the App to "Any
  account" and install it again on the other one.

## Where the code is

| Piece | Where |
| --- | --- |
| JWT, installations, token minting and cache | `internal/githubapp` |
| Which credential is live, and making it ambient | `internal/githubauth` |
| The git credential helper | `cmd/harness/githubcredential.go` |
| The endpoint the helper calls | `internal/httpapi/github.go`, [DATA-API.md](DATA-API.md) |
| The per-session `gh` token | `cmd/harness/github.go`, `internal/tools` `ExtraEnv` |
| Which account a session's clones belong to | `internal/workspace.GitHubOwners` |
