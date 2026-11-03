# The guard reads the whole event log for every bash call

`LoadGuardHistory` (`internal/session/guardstate.go`) reads the session's whole event log, and each compaction
ancestor's, for every `bash` call the autonomy guard screens. The cost of one check grows with the session. In
`shadow` mode the state is built before the command starts, so this time is added to the command. The model calls
and the commit run beside the command.

If a long session shows the cost, read only the event kinds the state needs.
