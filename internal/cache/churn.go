// Package cache implements the prompt-cache churn diagnostic from
// docs/CACHE.md: predict a sub-turn's cache miss from what the harness knows
// it appended, compare against the API's actual figure, and name the first
// message that differs when they disagree by more than the provider's churn
// tolerance (Split.Slack, docs/OBSERVED.md).
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// blockSize is the measured cache persistence interval for DeepSeek
// (docs/OBSERVED.md): hit = floor(common_prefix_tokens / 128) * 128.
// The prediction logic below is provider-agnostic; what the blocks are worth
// as tolerance is not, and lives in Split.Slack.
const blockSize = 128

// Report is the churn diagnostic for one sub-turn's request.
type Report struct {
	ExpectedMissTokens int
	ActualMissTokens   int
	Churned            bool
	// ChurnPointIndex is the index of the first message that differs from
	// the previous request, set only when Churned is true.
	ChurnPointIndex *int
}

// Split is one request's token accounting resolved onto the cache-hit and
// cache-miss counts by the provider seam — the same split the cost model
// and the stored usage payload use (internal/session/client.go,
// docs/KIMI-INTEGRATION.md §2). The Detector reads its inputs from this,
// never from wire.Usage: the raw usage's cache fields are provider-shaped —
// DeepSeek reports a hit/miss pair, Kimi K3 a single cached_tokens with the
// miss derived — so a detector that read them directly would compare its
// prediction against a field Kimi never populates (always zero) and every
// K3 verdict would be meaningless (docs/CACHE.md).
type Split struct {
	PromptTokens     int
	CacheHitTokens   int
	CacheMissTokens  int
	CompletionTokens int
	// Slack is the churn tolerance for the provider this split came from —
	// the largest miss over the prediction a healthy sub-turn may show
	// before the detector reports churn. It is an empirical bound on that
	// provider's over-prediction, not a property of its cache
	// (docs/OBSERVED.md): 127 for DeepSeek, the trailing partial 128-token
	// block; 512 for Kimi K3, the largest over-prediction across thirteen
	// observed sub-turns. The seam's client supplies it, the same way
	// UsageSplit decides how the raw usage becomes a hit/miss pair.
	Slack int
}

// Detector tracks one session's previous sub-turn so it can predict the
// next one's cache miss. It holds no state outside the session it belongs
// to; sharing a Detector across sessions would name the wrong message on a
// churn report (docs/CACHE.md).
type Detector struct {
	have bool
	// prevCacheableTokens is the previous request's prompt plus the tokens it
	// generated. The unit persisted by sub-turn N covers [system … assistant_N],
	// so the completion is inside the prefix the next request can hit, and
	// predicting from the prompt alone under-predicts the hit by about a block
	// (docs/CACHE.md).
	prevCacheableTokens int
	prevHashes          []string
}

// NewDetector returns an empty Detector for one session.
func NewDetector() *Detector {
	return &Detector{}
}

// NewDetectorFrom returns a Detector primed as though Observe had just been
// called for a sub-turn whose request was prevItems and whose usage
// totalled prevCacheableTokens (prompt tokens plus completion tokens).
// Resuming a session uses this so the churn check on the first sub-turn
// after resume compares against the session's real prior request instead of
// silently skipping it the way a fresh Detector would (docs/CACHE.md).
func NewDetectorFrom(prevCacheableTokens int, prevItems []wire.Item) *Detector {
	return &Detector{
		have:                true,
		prevCacheableTokens: prevCacheableTokens,
		prevHashes:          hashItems(prevItems),
	}
}

// Observe records this sub-turn's request and usage, returning the
// diagnostic against whatever the previous call to Observe recorded. split
// is the provider seam's cache-hit/cache-miss split for the request, never
// the raw wire.Usage (see Split); split.Slack must carry that provider's
// tolerance. The first call on a fresh Detector has nothing to compare
// against, so it reports the actual miss as fully expected.
func (d *Detector) Observe(items []wire.Item, split Split) Report {
	hashes := hashItems(items)

	var report Report
	if !d.have {
		report = Report{ExpectedMissTokens: split.CacheMissTokens, ActualMissTokens: split.CacheMissTokens}
	} else {
		expectedHit := (d.prevCacheableTokens / blockSize) * blockSize
		expectedMiss := split.PromptTokens - expectedHit
		if expectedMiss < 0 {
			expectedMiss = 0
		}
		report = Report{ExpectedMissTokens: expectedMiss, ActualMissTokens: split.CacheMissTokens}
		if split.CacheMissTokens > expectedMiss+split.Slack {
			report.Churned = true
			idx := firstDivergence(d.prevHashes, hashes)
			report.ChurnPointIndex = &idx
		}
	}

	d.have = true
	d.prevCacheableTokens = split.PromptTokens + split.CompletionTokens
	d.prevHashes = hashes
	return report
}

func hashItems(items []wire.Item) []string {
	out := make([]string, len(items))
	for i, m := range items {
		b, err := json.Marshal(m)
		if err != nil {
			out[i] = ""
			continue
		}
		sum := sha256.Sum256(b)
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

// firstDivergence returns the first index where prev and cur disagree, or
// the length of the shorter one if every shared index agrees — which means
// the prefix relationship itself broke (cur is not an extension of prev).
func firstDivergence(prev, cur []string) int {
	n := len(prev)
	if len(cur) < n {
		n = len(cur)
	}
	for i := 0; i < n; i++ {
		if prev[i] != cur[i] {
			return i
		}
	}
	return n
}
