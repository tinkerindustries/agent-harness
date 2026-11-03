# TestJobOutputConcurrentPollsDoNotPanicOrDuplicate fails intermittently

`TestJobOutputConcurrentPollsDoNotPanicOrDuplicate` (`internal/tools/bashbackground_test.go`) is timing
sensitive. In two of three full `scripts/build.sh` runs on a branch that did not touch `internal/tools`, it failed
with "the polls returned 0 bytes of output, want 1692". It passed alone ten times and in the whole
`internal/tools` package. The cause is undiagnosed.

Found while landing the guard's phase 2. Each failure costs a full gate rerun.

## Resolution

The test's final wait used `waitForBackgroundDone`, which returns only the last poll. Polls that land between the
shell's writes and its exit consume output, so those bytes never reached the test's total. With the CPUs busy the
original failed in 17 of 600 runs, including the reported "0 bytes of output, want 1692". The test now keeps the
result of every poll, and passes 600 of 600 under the same load. `job_output` itself loses nothing.
