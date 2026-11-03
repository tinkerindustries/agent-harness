# TestJobOutputConcurrentPollsDoNotPanicOrDuplicate fails intermittently

`TestJobOutputConcurrentPollsDoNotPanicOrDuplicate` (`internal/tools/bashbackground_test.go`) is timing
sensitive. In two of three full `scripts/build.sh` runs on a branch that did not touch `internal/tools`, it failed
with "the polls returned 0 bytes of output, want 1692". It passed alone ten times and in the whole
`internal/tools` package. The cause is undiagnosed.

Found while landing the guard's phase 2. Each failure costs a full gate rerun.
