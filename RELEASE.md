# Releasing

Cutting a release and deploying it are one sequence here, so they are one
document. There is no CI: nothing builds, publishes or deploys unless a person
runs the command. Every step below is manual and every step is reversible.

The deployment this document targets is the `deepseek-harness-prod` compose
project described in the README's "A production stack beside the dev one". It
runs on the same machine as the dev stack and shares nothing with it but the
docker daemon and the DeepSeek account.

## Versioning

Semver, as annotated `vX.Y.Z` tags on `main`.

- **Patch** — a fix that changes no request wording and no stored shape.
- **Minor** — new behaviour, new subcommand, new tool.
- **Major** — a change a caller has to react to: a removed MCP tool, a renamed
  field in a work request or result, an incompatible session store.

Anything touching the frozen head of a request — the system prompt, the tool
array, the request path — is at least a minor even when it reads like a fix,
because it invalidates the prompt cache for every session. `docs/CACHE.md` is
the reference.

`internal/mcp/service.go`'s `serverVersion` is the only version string in the
repo. It is what an MCP client sees in `serverInfo`, so it is bumped in the
release commit and must match the tag.

## What a release produces

| Artifact | Where it lives | Produced by |
| --- | --- | --- |
| `vX.Y.Z` tag | `origin` | `git tag`, below |
| GitHub release notes | github.com/mrgeoffrich/agent-harness/releases | `gh release create` |
| `deepseek-harness:prod-<sha>` | This machine's docker image store | `scripts/prod.sh promote` |
| `deepseek-harness:prod` | The same store; the tag prod runs | `promote` moves it |

Nothing is pushed to a registry. The deployable is a local image tag, which
means **a release exists only on the machine that built it** and a pruned image
store loses the rollback targets — see "If a deploy goes wrong".

## Before releasing

- [ ] `gofmt -l cmd internal` is silent and `go vet ./cmd/... ./internal/...`
      is clean. All three name the source roots: agent workspaces live inside
      this checkout, so a bare `./...` builds whatever Go a session left there
      ([TESTING.md](TESTING.md)).
- [ ] `scripts/test.sh` passes, and `npm --prefix web run test` if `web/`
      changed. The full sequence, cheapest first, is
      [TESTING.md](TESTING.md)'s "Smoke test after a change".
- [ ] The change has run for real on the dev stack — `docker compose up -d
      --build`, then a published run watched through to a result. The suites
      never call `api.deepseek.com`, so nothing else exercises the agent loop.
- [ ] `main` is pushed and your checkout is clean. `promote` refuses a dirty
      tree, because it builds the working tree rather than the tag.
- [ ] Prod has no run in flight you would mind interrupting:
      `scripts/prod.sh status` and the queue at <http://localhost:8180>.

## Cutting a release

```sh
git checkout main && git pull

# Bump serverVersion in internal/mcp/service.go to match the tag.
git commit -am "Release vX.Y.Z"
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin main --follow-tags

gh release create vX.Y.Z --generate-notes \
  --notes "$(git log --format='- %s' --no-merges --invert-grep --grep='^Release v' vX.Y.(Z-1)..HEAD)"
```

Pushing the tag triggers nothing. The release is not deployed until you promote
it.

## Deploying to production

```sh
scripts/prod.sh promote     # build this checkout, move the prod tag onto it
scripts/prod.sh deploy      # recreate the containers onto the new tag
scripts/prod.sh status      # containers, deployed image, queue health
```

Three things worth knowing about that sequence:

**`promote` builds the working tree, not the tag.** It reads `git rev-parse
--short HEAD` only to name the image. Deploying a tag means being checked out at
it first — which the sequence above already is, having just made it.

**`deploy` is the only step that changes what is serving.** `promote` moves a
tag in the local image store and leaves the running containers alone, so the two
can be minutes apart. `deploy` passes `--force-recreate`, which is needed
because compose will not replace a container whose service definition is
unchanged and only the image behind the tag has moved.

**`deploy` drains rather than cuts.** `serve` handles SIGTERM by finishing
in-flight runs, and the compose file gives it 60 seconds, so a deploy can take
about that long. A run that does not finish in time leaves its queue row leased
and is redelivered to the new container once the lease expires. The
`harness-data` volume is untouched by a deploy, so sessions and any queue
backlog survive it.

Then confirm it landed:

```sh
scripts/prod.sh status                      # the prod tag points at the new image
curl -sf localhost:8180/api/queue           # {"available":true,"halted":false}
curl -s localhost:8180/api/settings         # deepseek.api_key shows set and masked
scripts/prod.sh logs harness | grep -i balance   # the existing check; a good balance confirms the key works
```

Open <http://localhost:8180> for the session list, and re-run a real work
request through the MCP server on `http://127.0.0.1:8180/mcp` if the change
touched the agent loop.

## Changelog

There is no `CHANGELOG.md`. The release page is the changelog, which makes the
commit subjects the changelog — write them for someone reading that page.

Getting them onto it takes both flags in the command above. `--generate-notes`
alone is not enough: GitHub's "What's Changed" list is built from **pull
requests merged in the range**, not from commits, and most work here is pushed
straight to `main`. A release with no PRs behind it generates a body of exactly
one line — the compare link — and says nothing about what shipped. v0.42.0
through v0.45.0 are all bare that way. Keep the flag for that compare link and
for the releases that do have PRs, and prepend the subjects with `--notes`.

`--invert-grep` drops the `Release vX.Y.Z` commit, the one commit in the range
that tells a reader nothing.

## Hotfix

For a fix that should ship without dragging in whatever else has landed on
`main`:

```sh
git checkout -b hotfix-vX.Y.Z vX.Y.(Z-1)
git cherry-pick <sha>
# Bump serverVersion, run the smoke sequence.
git commit -am "Release vX.Y.Z"
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin hotfix-vX.Y.Z --follow-tags

scripts/prod.sh promote && scripts/prod.sh deploy
```

`promote` names the image from the detached commit, so the `prod-<sha>` tag
still identifies exactly what was built. Merge the branch back into `main`
afterwards, or the next release silently reverts the fix.

## If a deploy goes wrong

Roll back to a previously promoted image. The immutable `prod-<sha>` tags are
what makes this fast — no rebuild, no checkout:

```sh
docker images 'deepseek-harness:prod-*'     # what is available to go back to
scripts/prod.sh rollback <sha>
scripts/prod.sh deploy
```

`rollback` with no argument lists the candidates and exits.

Constraints worth knowing before you need them:

- **The rollback targets are local docker images and nothing else.** They are
  not in a registry and not in git. `docker image prune -a`, or Docker Desktop
  reclaiming space, deletes them; then rolling back means checking out the old
  tag and running `promote`, which is a full rebuild.
- **The tag and the GitHub release stay.** Rolling back the deployment does not
  un-cut the release. Either ship a `vX.Y.Z+1` that supersedes it, or
  `gh release delete vX.Y.Z` and `git push --delete origin vX.Y.Z` if it was
  never really out.
- **State does not roll back with the image.** A release that changed the shape
  of anything in the SQLite store leaves that change behind in the
  `harness-data` volume when you go back. There is no migration tooling here,
  so a schema change is a one-way deploy in practice — treat it as major and
  be sure before promoting.
- **The dev stack is not a rollback path.** Different project, different volumes,
  different ports. Bringing dev up does not restore production.
