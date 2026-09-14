package settings

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Type is the value type of a setting, carried on its registry descriptor so
// every caller renders and validates it the same way instead of
// re-inferring it from the key.
type Type int

const (
	// TypeString is a plain text value (a model name, an API key).
	TypeString Type = iota
	// TypeInteger is a whole number.
	TypeInteger
	// TypeDuration is a time span stored as text in the settings table and
	// parsed on read ("30s", "10m", "1h").
	TypeDuration
)

// String returns the type's wire name, what the HTTP payload and the screen
// see.
func (t Type) String() string {
	switch t {
	case TypeInteger:
		return "integer"
	case TypeDuration:
		return "duration"
	default:
		return "string"
	}
}

// Setting groups, in registry order.
const (
	GroupCredentials = "Credentials"
	GroupRunBudget   = "Run budget"
	GroupToolLimits  = "Tool limits"
	GroupModels      = "Models"
)

// Setting keys. The registry below is the single source of truth; these
// constants exist so Go call sites name a key without string literals.
const (
	KeyDeepSeekAPIKey            = "deepseek.api_key"
	KeyKimiAPIKey                = "kimi.api_key"
	KeyGoogleAPIKey              = "google.api_key"
	KeyGoogleVisionModel         = "google.vision_model"
	KeyGoogleVisionThinkingLevel = "google.vision_thinking_level"

	KeyRunMaxTokens                 = "run.max_tokens"
	KeyRunDeadline                  = "run.deadline"
	KeyRunCompactionThreshold       = "run.compaction_threshold"
	KeyRunCompactionThresholdKimiK3 = "run.compaction_threshold_kimi_k3"
	KeyRunStopGracePeriod           = "run.stop_grace_period"

	KeyToolOutputCap                 = "tools.output_cap"
	KeyToolBashTimeout               = "tools.bash_timeout"
	KeyToolBashTimeoutMax            = "tools.bash_timeout_max"
	KeyToolBashWaitDelay             = "tools.bash_wait_delay"
	KeyToolTimeout                   = "tools.tool_timeout"
	KeyToolWebFetchTimeout           = "tools.webfetch_timeout"
	KeyToolTaskTimeout               = "tools.task_timeout"
	KeyToolReviewScreenshotTimeout   = "tools.reviewscreenshot_timeout"
	KeyToolScreenshotTimeout         = "tools.screenshot_timeout"
	KeyToolTranscribeTimeout         = "tools.transcribe_timeout"
	KeyToolTranscribeMaxChunks       = "tools.transcribe_max_chunks"
	KeyToolTranscribeConcurrency     = "tools.transcribe_concurrency"
	KeyToolWebFetchMaxBody           = "tools.webfetch_max_body"
	KeyToolWebFetchMaxExtract        = "tools.webfetch_max_extract"
	KeyToolReviewScreenshotMaxImages = "tools.reviewscreenshot_max_images"
	KeyToolReviewScreenshotMaxBytes  = "tools.reviewscreenshot_max_bytes"
	KeyToolAttachmentsMaxCount       = "tools.attachments_max_count"
	KeyToolAttachmentsMaxBytes       = "tools.attachments_max_bytes"
	KeyToolMCPTimeout                = "tools.mcp_timeout"

	KeyDefaultModel      = "model.default"
	KeyDefaultFlashModel = "model.flash"
	KeyDefaultEffort     = "model.effort"
)

// Descriptor is one registry entry: everything the harness knows about a
// setting. Validation lives here — one bound, enforced in Go on every write
// path (CLI, HTTP PUT, screen), never re-implemented in the UI.
type Descriptor struct {
	Key   string
	Group string
	Type  Type
	// Default is the canonical text form of the setting's value. An
	// installation that stores nothing resolves to this; the values below
	// are exactly the compile-time constants the limits used to be.
	Default string
	// Min and Max are the inclusive validation bounds: the integer value for
	// TypeInteger, nanoseconds for TypeDuration.
	Min int64
	Max int64
	// Allowed, when non-empty, is the only accepted set of values for a
	// TypeString setting.
	Allowed     []string
	Description string
	// Secret marks a setting that holds a credential, so a caller printing
	// settings masks it rather than showing the value.
	Secret bool
	// Restart marks a setting read once at startup rather than on every
	// call. Nothing carries it today.
	Restart bool
}

// registry is the ordered list of every setting. Groups run together in
// order: Credentials, Run budget, Tool limits, Models.
var registry = []Descriptor{
	// --- Credentials ---
	stringSetting(KeyDeepSeekAPIKey, GroupCredentials,
		"DeepSeek API key — the harness's own account", "", true, false),
	stringSetting(KeyKimiAPIKey, GroupCredentials,
		"Kimi API key — Moonshot AI account used for kimi-k3 runs (third_party/kimi-docs/api/overview.md)", "", true, false),
	stringSetting(KeyGoogleAPIKey, GroupCredentials,
		"Google API key — sent to Gemini by the vision tools (Glance, Ground, Detect)", "", true, false),

	// --- Run budget ---
	intSetting(KeyRunMaxTokens, GroupRunBudget, 48000, 1, 1_000_000,
		"Default max output tokens for a run that omits max_tokens. The real ceiling is DeepSeek's output limit; this is the harness's own default."),
	durationSetting(KeyRunDeadline, GroupRunBudget, "1h", time.Second, 365*24*time.Hour,
		"Wall clock a request that omits deadline_ms gets."),
	intSetting(KeyRunCompactionThreshold, GroupRunBudget, 768*1024, 1024, 1_000_000,
		"Prompt-token threshold at which the session compacts its history (DeepSeek's recommended Claude Code compaction window, 768K of the 1M context)"),
	intSetting(KeyRunCompactionThresholdKimiK3, GroupRunBudget, 128*1024, 1024, 1_000_000,
		"Prompt-token threshold at which a kimi-k3 session compacts its history, replacing run.compaction_threshold for that model. K3 cache-miss input costs $3.00/M against deepseek-flash's $0.15/M off-peak — about 20x (configs/prices.json) — so the global 768K threshold would expose a K3 cold start or churn to a ~$2.30 full-prompt miss. 128K is unchanged from when this was scaled against deepseek-v4-pro (about 7x K3's rate): a worst-case full-prompt miss at 128K still costs ~$0.39, but that is now roughly 3x deepseek-flash's own ~$0.12 at 768K off-peak, not the same order it was against deepseek-v4-pro's ~$0.34. DeepSeek cut deepseek-flash's rate on 2026-09-10; this threshold has not been re-scaled against it (docs/KIMI-INTEGRATION.md §3)."),
	durationSetting(KeyRunStopGracePeriod, GroupRunBudget, "30s", time.Second, time.Hour,
		"How long a stop waits for a cancelled run to return before it gives up on the goroutine and finishes the run without it (docs/RUN-CONTROL.md \"Half two\"). Sized against the longest uninterruptible thing a healthy run does between context checks, not against sub-turn latency."),

	// --- Tool limits ---
	intSetting(KeyToolOutputCap, GroupToolLimits, 200_000, 1000, 10_000_000,
		"Output byte cap for every tool result, with truncation labelled in the result"),
	durationSetting(KeyToolBashTimeout, GroupToolLimits, "2m", time.Second, 24*time.Hour,
		"Default wall-clock timeout for a Bash call that omits timeout"),
	durationSetting(KeyToolBashTimeoutMax, GroupToolLimits, "10m", time.Second, 24*time.Hour,
		"Ceiling a Bash call's requested timeout is clamped to"),
	durationSetting(KeyToolBashWaitDelay, GroupToolLimits, "2s", 100*time.Millisecond, 5*time.Minute,
		"How long a Bash call keeps waiting for a command's output pipes to close after the command exits or is cancelled, before it stops waiting and kills the process group. A command that backgrounds a process without redirecting its output holds the pipes open after the shell exits; this bounds that wait so the call cannot hang, and the process group kill takes the orphan with it."),
	durationSetting(KeyToolTimeout, GroupToolLimits, "30s", time.Second, 24*time.Hour,
		"Default wall-clock timeout for every other tool"),
	durationSetting(KeyToolWebFetchTimeout, GroupToolLimits, "45s", time.Second, 24*time.Hour,
		"Wall-clock timeout for one WebFetch call"),
	durationSetting(KeyToolTaskTimeout, GroupToolLimits, "10m", time.Second, 24*time.Hour,
		"Wall-clock timeout for one Task subagent call"),
	durationSetting(KeyToolReviewScreenshotTimeout, GroupToolLimits, "120s", time.Second, 24*time.Hour,
		"Wall-clock timeout for one Gemini call made by Glance, Ground, or Detect (describing or locating something routinely takes longer than the 30-second tool default, and a whole-page description with thinking on has been measured past 60). The key keeps the name of the tool it originally bounded; Crop makes no model call and is not covered by it."),
	durationSetting(KeyToolScreenshotTimeout, GroupToolLimits, "90s", time.Second, 24*time.Hour,
		"Wall-clock timeout for one Screenshot capture (launching Chromium, navigating, waiting for the page to settle and encoding the image). The driver's own navigation timeout is derived from this, so raise it for an application that is slow to start rather than retrying the call."),
	durationSetting(KeyToolTranscribeTimeout, GroupToolLimits, "300s", time.Second, 24*time.Hour,
		"Wall-clock timeout for one Transcribe call, covering every chunk of the image. The chunks are sent concurrently, so this is a few of the per-call vision timeouts rather than the sum of them all — but it has to bound the slowest wave of a tall page, not one request, which is why it is its own key and not the Glance one."),
	intSetting(KeyToolTranscribeMaxChunks, GroupToolLimits, 24, 1, 200,
		"Maximum chunks one Transcribe call cuts an image into, and so the maximum vision calls it bills. An image that would need more is not refused: the split stops and the last chunk keeps the remainder, which the per-chunk heights in the result make visible."),
	intSetting(KeyToolTranscribeConcurrency, GroupToolLimits, 4, 1, 32,
		"How many of a Transcribe call's chunks are in flight at once. Raising it shortens a tall page's wall time and raises the burst rate against the vision API; the whole call is still bounded by tools.transcribe_timeout."),
	intSetting(KeyToolWebFetchMaxBody, GroupToolLimits, 4<<20, 1024, 1<<30,
		"Bytes of a fetched page read before extraction"),
	intSetting(KeyToolWebFetchMaxExtract, GroupToolLimits, 40_000, 100, 10_000_000,
		"Bytes of extracted text sent to the flash summarising call"),
	intSetting(KeyToolReviewScreenshotMaxImages, GroupToolLimits, 4, 1, 100,
		"Maximum images one Glance call accepts (the key keeps the name of the tool it originally bounded)"),
	intSetting(KeyToolReviewScreenshotMaxBytes, GroupToolLimits, 5<<20, 1024, 1<<30,
		"Maximum bytes per image file for Glance, Ground, and Detect (5 MB at the default; the key keeps the name of the tool it originally bounded)"),
	intSetting(KeyToolAttachmentsMaxCount, GroupToolLimits, 8, 1, 100,
		"Maximum image attachments one request may carry"),
	intSetting(KeyToolAttachmentsMaxBytes, GroupToolLimits, 5<<20, 1024, 1<<30,
		"Maximum bytes per image attachment, matching the vision tools' per-file cap so an attachment can always be looked at (5 MB at the default)"),
	durationSetting(KeyToolMCPTimeout, GroupToolLimits, "120s", time.Second, 24*time.Hour,
		"Wall-clock timeout for one MCP tool call (docs/MCP.md). Longer than the 30-second default for the harness's own tools: the calls that motivated MCP support drive external applications — rendering a viewport, driving a browser — which routinely run past that default."),

	// --- Models ---
	stringSetting(KeyDefaultModel, GroupModels,
		"Default model for runs that omit model", "deepseek-flash", false, false),
	stringSetting(KeyDefaultFlashModel, GroupModels,
		"Model Task subagents and WebFetch summarisation run", "deepseek-flash", false, false),
	stringSetting(KeyDefaultEffort, GroupModels,
		"Default reasoning effort for runs that omit effort", "high", false, false).
		withAllowed("low", "high", "max"),
	stringSetting(KeyGoogleVisionModel, GroupModels,
		"Gemini model the vision tools (Glance, Ground, Detect) send images to. Defaults to gemini-3.7-flash, which bills the same input as 3.5 Flash and 2.4x less output ($3.75/M against $9.00/M, configs/prices.json) while being the newer model at the thing these tools do. Its rates are introductory and double on 2027-01-01. A model with no entry in the price table still runs — the cost lookup fails and the caller keeps zero (internal/tools/vision.go), so its spend silently vanishes from every figure in the UI rather than erroring. Add the entry before changing this.", "gemini-3.7-flash", false, false),
	stringSetting(KeyGoogleVisionThinkingLevel, GroupModels,
		"How hard the vision model thinks before answering a Glance, Ground, or Detect call. Thinking bills at the output rate and is where a call's cost goes — an empty findings list has been measured at 1,947 thinking tokens against one token of answer. \"auto\" lets the tool choose per call: medium for Glance's default description or a query, low for Ground and Detect, which are locating rather than reasoning. \"minimal\" is not offered: gemini-3.7-flash, the default vision model, refuses it with a 400 naming the three it takes, so pinning it here would fail every Glance, Ground, and Detect call (internal/gemini.LevelsFor).", "auto", false, false).
		withAllowed("auto", "low", "medium", "high"),
}

func stringSetting(key, group, description, def string, secret, restart bool) Descriptor {
	return Descriptor{Key: key, Group: group, Type: TypeString, Default: def, Description: description, Secret: secret, Restart: restart}
}

func intSetting(key, group string, def, min, max int64, description string) Descriptor {
	return Descriptor{Key: key, Group: group, Type: TypeInteger, Default: strconv.FormatInt(def, 10), Min: min, Max: max, Description: description}
}

// durationSetting stores its default as text ("1h") so the operator sees
// exactly the value an unset key resolves to; def must parse as a duration
// (a programmer error otherwise, caught at startup).
func durationSetting(key, group, def string, min, max time.Duration, description string) Descriptor {
	if _, err := time.ParseDuration(def); err != nil {
		panic(fmt.Sprintf("settings: %s default %q is not a duration: %v", key, def, err))
	}
	return Descriptor{Key: key, Group: group, Type: TypeDuration, Default: def, Min: int64(min), Max: int64(max), Description: description}
}

func (d Descriptor) withRestart() Descriptor {
	d.Restart = true
	return d
}

func (d Descriptor) withAllowed(values ...string) Descriptor {
	d.Allowed = values
	return d
}

// ValidKeys lists every known setting key in registry order. It is derived
// from the registry, never hand-maintained.
var ValidKeys = func() []string {
	keys := make([]string, len(registry))
	for i, d := range registry {
		keys[i] = d.Key
	}
	return keys
}()

// Lookup returns key's descriptor and whether it is a known setting.
func Lookup(key string) (Descriptor, bool) {
	for _, d := range registry {
		if d.Key == key {
			return d, true
		}
	}
	return Descriptor{}, false
}

// Descriptors returns a copy of the registry, in order.
func Descriptors() []Descriptor {
	out := make([]Descriptor, len(registry))
	copy(out, registry)
	return out
}

// IsSecretKey reports whether key holds a credential worth masking.
// Derived from the registry.
func IsSecretKey(key string) bool {
	d, ok := Lookup(key)
	return ok && d.Secret
}

// CompactionKeyForModel returns the settings key whose per-model value
// replaces run.compaction_threshold for model — run.compaction_threshold_kimi_k3
// for kimi-k3 — and whether model has an override. A model without an entry
// resolves the global key. The table is the run-budget counterpart of the
// model→provider table in internal/provider (docs/KIMI-INTEGRATION.md §4.3).
func CompactionKeyForModel(model string) (string, bool) {
	switch model {
	case "kimi-k3":
		return KeyRunCompactionThresholdKimiK3, true
	}
	return "", false
}

// validate rejects value unless it fits the descriptor's type and bounds.
// This is the one place a setting value is checked: the HTTP PUT and the
// screen's save both land here, so a rejected value reads identically from
// every surface.
func (d Descriptor) validate(value string) error {
	switch d.Type {
	case TypeInteger:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not an integer", d.Key, value)
		}
		if n < d.Min || n > d.Max {
			return fmt.Errorf("%s: %d is out of range [%d, %d]", d.Key, n, d.Min, d.Max)
		}
	case TypeDuration:
		dur, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: %q is not a duration (e.g. 30s, 10m, 1h)", d.Key, value)
		}
		if dur < time.Duration(d.Min) || dur > time.Duration(d.Max) {
			return fmt.Errorf("%s: %s is out of range [%s, %s]", d.Key, dur, time.Duration(d.Min), time.Duration(d.Max))
		}
	case TypeString:
		if len(d.Allowed) > 0 {
			for _, allowed := range d.Allowed {
				if value == allowed {
					return nil
				}
			}
			return fmt.Errorf("%s: %q must be one of %s", d.Key, value, strings.Join(d.Allowed, ", "))
		}
	}
	return nil
}

// ValidationError reports a well-formed key whose value fails the registry's
// type or bounds check. It is distinct from UnknownKeyError so the HTTP
// layer can answer 400 for both a typo'd key and a rejected value.
type ValidationError struct {
	Key   string
	Value string
	Err   error
}

func (e ValidationError) Error() string { return e.Err.Error() }
func (e ValidationError) Unwrap() error { return e.Err }
