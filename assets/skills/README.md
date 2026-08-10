# Skills we ship

Claude Code skills that drive this harness from the outside. They are assets
for distribution, not skills this repository uses: nothing here is loaded by
being checked in, and `internal/skills` does not see this directory when a run
clones the repo.

Install one by copying it into a skills directory:

    cp -R assets/skills/<name> ~/.claude/skills/     # personal, any repo
    cp -R assets/skills/<name> .claude/skills/       # a project, checked in

| Skill | What it does |
| --- | --- |
| `deepseek-flash-task` | Delegates one coding task to a flash run over the MCP server, on `full` permissions, and collects a schema-validated report. Branches off whatever base you name, pushes, opens a draft PR. |
| `deepseek-harness-wait` | Waits a run out without spending a turn per check: a background status poll against the read-only API, one `deepseek_result` call at the end, then verification of the branch and PR the report claims. Picks up where `deepseek-flash-task` leaves off. |
| `deepseek-flash-plan` | Spreads work too big for one run across a chain of phases: one integration branch off main, a `deepseek-flash-task` per phase based on that branch, each merged before the next launches, one pull request to main at the end. Calls the other two rather than repeating them. |

They depend on details of this repository. `deepseek-flash-task`'s result schema
matches the subset of JSON Schema that `internal/tools/schema.go` validates, and
its prompt template works around the container having no git identity and
authenticating to GitHub over https only. `deepseek-harness-wait` encodes the
published ports, the `running`/`ok`/`failed`/`timeout` values of
`store.Status*`, and the response shape of
`GET /api/requests/{request_id}/status`. It also names the version that endpoint
arrived in, because the recipe it teaches does not work against an older
harness. Changing any of that means changing the skill.

`deepseek-flash-plan` depends on the other two instead, and is the one to
install last: on its own it describes a workflow whose every step is a call into
`deepseek-flash-task`. The harness detail it does rest on is that
`repos[].branch` reaches `git clone --branch` (`internal/workspace/clone.go`),
which is what lets a phase start from a branch an earlier phase pushed rather
than from the repository's default.
