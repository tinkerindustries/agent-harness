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
| `deepseek-flash-task` | Delegates a coding task to a flash run over the MCP server, on `full` permissions, and collects a schema-validated report. Branches off main, pushes, opens a draft PR. |

`deepseek-flash-task` depends on details of this repository: the result schema
matches the subset of JSON Schema that `internal/tools/schema.go` validates,
and the prompt template works around the container having no git identity and
authenticating to GitHub over https only. Changing either means changing the
skill.
