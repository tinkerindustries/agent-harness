// Package cache implements the prompt-cache churn diagnostic from
// docs/CACHE.md: predict a sub-turn's cache miss from what the harness knows
// it appended, compare against the API's actual figure, and name the first
// message that differs when they disagree by more than the 128-token block
// slack.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// blockSize is the measured cache persistence interval (docs/OBSERVED.md):
// hit = floor(common_prefix_tokens / 128) * 128.
const blockSize = 128

// blockSlack is the largest miss a well-behaved request can show over the
// prediction: the trailing partial block, always under one block.
const blockSlack = blockSize - 1

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
// called for a sub-turn whose request was prevMessages and whose usage
// totalled prevCacheableTokens (prompt tokens plus completion tokens).
// Resuming a session uses this so the churn check on the first sub-turn
// after resume compares against the session's real prior request instead of
// silently skipping it the way a fresh Detector would (docs/CACHE.md).
func NewDetectorFrom(prevCacheableTokens int, prevMessages []wire.Message) *Detector {
	return &Detector{
		have:                true,
		prevCacheableTokens: prevCacheableTokens,
		prevHashes:          hashMessages(prevMessages),
	}
}

// Observe records this sub-turn's request and usage, returning the
// diagnostic against whatever the previous call to Observe recorded. split
// is the provider seam's cache-hit/cache-miss split for the request, never
// the raw wire.Usage (see Split). The first call on a fresh Detector has
// nothing to compare against, so it reports the actual miss as fully
// expected.
func (d *Detector) Observe(messages []wire.Message, split Split) Report {
	hashes := hashMessages(messages)

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
		if split.CacheMissTokens > expectedMiss+blockSlack {
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

func hashMessages(messages []wire.Message) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
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
