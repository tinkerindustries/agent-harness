// Package gemini_test holds the one assertion that needs to see both
// internal/gemini and internal/session: that *gemini.Client satisfies
// session.Client, the seam StreamChatCompletion, CreateChatCompletion,
// UsageSplit, CacheSlack, IsReasoningStarved and RepairArguments together
// make up (docs/GEMINI-INTEGRATION.md §5.1). It lives in its own
// externally-named test package rather than package gemini's own test files
// because internal/session already imports internal/gemini for the vision
// client (internal/session/runner.go's Runner.Gemini field) — importing
// internal/session back from inside package gemini would be a cycle, but a
// separate gemini_test package importing both is not one, since nothing
// imports gemini_test in turn.
package gemini_test

import (
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/session"
)

func TestClientSatisfiesSessionClient(t *testing.T) {
	var _ session.Client = (*gemini.Client)(nil)
}
