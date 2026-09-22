# Finished plans

A plan whose phases have all landed, and the feature memory that went with it. Both are
kept as they were written: a plan is not updated once the work is done, and a memory here
describes the feature as the sessions building it understood it.

Read one of these to find out what a feature set out to do and in what order it was built.
For how the code behaves now, read [../../../ARCHITECTURE.md](../../../ARCHITECTURE.md) and
[../../../internal/CLAUDE.md](../../../internal/CLAUDE.md); for why it came out differently
from the plan, [../../DESIGN.md](../../DESIGN.md).

The reports stay in [../reports/](../reports/) under their feature's slug, whether or not
the plan has been archived.

A plan moves here when every phase it names has a report and the work is on `main`. Move
`<slug>.md` to `archive/<slug>.md` and `memory/<slug>.md` to `archive/memory/<slug>.md`,
then grep the whole repository for the slug and repoint every path naming either file.
