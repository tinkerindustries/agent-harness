# The guard version depends on how the parent encodes the bundle

`Bundle.Version()` hashes the bytes of `stage1.questions` and `stage2.schema` as the harness receives them, after
compaction of whitespace. A serialiser that escapes `<`, `>` and `&` (Go's `json.Marshal` does, JavaScript's
`JSON.stringify` does not) therefore gives a different version for the same logical bundle.

One parent sending one bundle is unaffected. Two parents, or one parent that changes its serialiser, record
different versions for the same content, which makes decisions harder to compare across them. If it matters, hash a
canonical re-encoding of those two objects, with the escaping fixed, instead of the received bytes.

## Resolution

Gone with the thing it described. The policy bundle and its version hash left this repository when the guard
became a client of the parent's `POST /classify` endpoint (`docs/STDIO-PROTOCOL.md`, "The autonomy guard"). Nothing
here hashes a bundle any more, and the endpoint's own `guard_version` is the endpoint's to keep stable.
