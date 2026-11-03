# The guard reads the whole event log for every bash call

`LoadGuardHistory` (`internal/session/guardstate.go`) reads the session's whole event log, and each compaction
ancestor's, for every `bash` call the autonomy guard screens. The cost of one check grows with the session. In
`shadow` mode the state is built before the command starts, so this time is added to the command. The model calls
and the commit run beside the command.

If a long session shows the cost, read only the event kinds the state needs.

## Resolution

`LoadGuardHistory` now reads only the tool call, tool result and tool denied events, filtered in SQL, so a check no
longer pulls reasoning, content, usage or streamed output off the disk. It still reads those three kinds from the
start of the log on every `bash` call. Tool results carry the command output, so the cost still grows with the
session. An incremental read that keeps the history between calls would remove that, and nothing has measured the
need for it.
