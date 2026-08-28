# Decision record — adopting per-worktree environments

[wt.md](wt.md) is the reference for what the tooling does and `wt.yaml` is the
contract. This file is the memory: why each choice was made, so that six
months from now the spec is not the only thing left explaining itself.

Adopted 2026-08-28, replacing `harness worktree`, this repo's own allocator.

## C1 — what isolates by default

The driver question is whether the shared state is something a worktree's user
*wants* to reach. For this repo the answer is almost always no: a worktree is
a place to develop the harness, and a run started there should not appear in
another worktree's session list, write into another worktree's database or
claim work queued for another stack.

So everything the repo owns isolates, and the isolation is almost free. The
compose project name carries the containers, the `_default` network and the
`harness-data` volume that holds the whole SQLite store; the workspace root is
a directory inside the worktree. Two ports and one project name is the entire
allocation.

The exceptions are in the `shared:` block, and none of them are state this
repo owns: the host docker socket, the production stack, the Unity CLI's
sign-in volume and image, the DeepSeek account, the language caches. Each is
either something isolating would cost far more than it is worth (Docker-in-
Docker per worktree) or something that is not local state at all (one account,
one installation).

## C2 — seeding mode

The work here is *through* the store, not on its schema — someone developing
the harness wants a stack that runs, not a copy of yesterday's sessions. So no
resource is seeded from a shared source. The workspace root starts empty:
another worktree's sessions are that worktree's, and re-running is cheap.

The credentials are the exception, and they are not a spec resource at all —
they live in the store, inside the compose volume, and there is nothing to
seed until compose has run once. That is why they are a `seed` hook rather
than a `state-path` with a `seed:` block, and why the hook runs after `start`.
Without it every new worktree begins by asking for the same DeepSeek key and
the same GitHub App private key that every other worktree already has, which
is the kind of friction that gets worked around badly.

## C3 — descriptor format

JSON. The old descriptor was XML (`.worktree-env.xml`), which was a local
choice with nothing else behind it; every other machine-readable thing in this
repo is JSON, and `encoding/json` is what any Go that reads it would reach
for. The tripwire hook reads it with `sed` and `awk` either way.

## C4 — config delivery

Environment, through `.env`. Every entry point already reads it:
`docker-compose.yml` interpolates `${VAR:-default}`, `config.LoadDotEnv` feeds
`harness serve` on the host, and `web/vite.config.ts` calls `loadEnv` on the
repository root. Nothing resolves a port natively, so there was no case for a
generated reader — `emit.reader` is deliberately absent from the spec.

## C7 — base branch

`main`, and only `main`. There is no stacked or dependent work in this
repository; branches are cut from `main`, carry one PR, and are merged and
deleted. The create skill says so explicitly and points at the manual route
for the review-feedback case, because branching from `main` to "resume" a
branch is how review feedback gets silently discarded.

## C8 — cleanup posture

`wt rm` owns it. An idle worktree here is expensive — a running container, a
named volume and a workspace root, times however many are left lying about —
so leaving them to accumulate is not free the way an idle checkout is. But
the removal is explicit, never automatic: the remove skill is the only thing
that tears one down, and a failed run is a reason to leave the worktree
alive, not to clean it up.

## C9 — slot ceiling

Eight. Each slot is a full harness stack: a container, a volume, a workspace
root and two ports. Eight of those running at once is already more than this
machine wants; the port space would allow far more and that is not the
binding constraint. Both bands are eight wide as a result.

## C6 — the enforcement hook

Enforced, and committed. This repository dispatches agent runs that nobody is
watching — `assets/skills/deepseek-flash-task` and
`deepseek-flash-autopilot` — which is precisely the case the guard exists
for: a run working in one worktree writing into another one, or into the main
checkout, with no human at the keyboard to notice.

Committing it makes the choice for everyone who clones the repo, which is why
it is recorded here rather than left implicit. The hook fails open — a missing
`wt`, an unreadable descriptor, any undefined exit code lets the call through
and says so on stderr — because a broken guard must not make the repository
unworkable.

## Removal posture

The spec has no `removal:` block, so the defaults stand: refuse on
uncommitted changes, warn on unpushed commits, warn on an open PR.

Stricter was considered and declined. It would be right if every branch here
always had an upstream and always carried a PR, and most do — but not all: a
throwaway worktree for a docs change or a spike is ordinary here, and making
those unremovable trades a real cost for a hypothetical one. `wt cleanup` and
the coordinator's sweep still require a merged pull request whatever this
block says, so the automatic path is unaffected either way.

## Worktree location

`.claude/worktrees/{slug}` — spelled out in the spec rather than left to wt's
default, which happens to be the same string today. It is where the retired
allocator put them and where Claude Code's own `isolation: "worktree"` creates
them, so the manual flow and the automatic one land in one place and no
existing tree has to move. The directory is gitignored, anchored, so the main
checkout is not untracked-dirty from the first worktree on.

## Port bands

`harness_http=8700` and `vite=5700` — the same bases the retired allocator
used, chosen so that no worktree created before the adoption is on a port the
new tooling would hand to a different one. The last digit of a port is its
slot, which was worth keeping: it makes `docker ps` and `lsof` output legible
without cross-referencing anything.

`reserved:` names 8080, 5173 and 8180. The first two are the main checkout's
own committed defaults, which slot 0 keeps. 8180 is the production stack. All
three also hold host-global reservations in the band ledger, which is the
difference that matters: the `reserved:` line stops *this* repo allocating
them, and the ledger reservation stops any other adopted repo doing it.

## What the import moved, and what was deleted

`harness worktree` allocated slots in `~/.deepseek-harness/worktrees.json` and
wrote `.worktree-env.xml`. Nothing was migrated from it, because at adoption
there was nothing live to migrate: its one registry entry was an orphan from a
worktree whose work had already merged, and the one real worktree on disk had
never run `init` at all.

The command and `internal/worktree` were deleted immediately after the
adoption was proved — around 1,500 lines, plus the registry bind mount and
`HARNESS_REGISTRY_DIR` from both compose files, which existed only so that
allocator could run inside a container. `~/.deepseek-harness/` is no longer
read or written by anything in this repo.

`harness worktree seed` was the piece worth keeping. Its logic moved to
`harness seed-credentials`, a top-level command with `-export` and `-import`
halves, each running inside the stack it touches. It is top-level because it
is not about worktrees: it is about two harness installations, one of which
has credentials the other needs. The worktree case is only the usual reason
for there to be two.

## The container-side client

Not yet built, and the one open piece of this adoption.

An agent session inside the harness container clones the target repo and, when
that target is this repo, runs `docker compose up` against the *host's* daemon
through the shared socket. That binds real host ports under a real host
compose project name — potentially those of the stack currently running the
agent. `harness worktree init -standalone` handles it today by allocating
against the registry file, which `docker-compose.yml` bind-mounts into both
stacks.

The replacement is for the container to be a `wt` client: `wt` on the image,
`wtd`'s loopback TCP listener enabled with a token, and `WT_ENDPOINT`,
`WT_CLIENT_TOKEN` and `WT_CLIENT_EPHEMERAL=1` in the harness service's
environment. Ephemeral is the right flag — an agent's clone is disposable and
its entry should be reclaimable. The socket cannot be shared into the
container, which is what the TCP listener exists for.

Two things have to be decided before it can be built. `wt` has to reach the
Alpine image, and `tinkerindustries/worktree-manager` is private, so that is
either a release asset fetched with a build-time token or a binary vendored
some other way. And enabling the TCP listener re-registers `wtd` on the host,
which changes the developer's own daemon setup rather than anything in this
repository.

`harness worktree init -standalone` was the interim answer, and it has been
removed rather than kept until the replacement exists. That was a deliberate
call and it has a cost worth stating plainly: between the removal and the
container-side client, an agent session that brings a stack up in its own
clone has no allocator at all. It sets the compose project name and ports by
hand, from what `docker ps` shows the host has published — the same thing the
`deepseek-flash-task` skill already told sessions to do in any repository with
no tooling of its own, now true of this one too.

The risk that carries is a session that sets nothing. Compose then falls back
to the clone's directory basename, which for a clone of this repo is the host
dev stack's own project name, and the session recreates the containers of the
stack running it. The skill is emphatic about it for that reason. It is a
weaker guarantee than an allocator, and it is the state of things until the
container is a `wt` client.

