package settings

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Type is the value type of a setting, carried on its registry descriptor so
// harness config, the HTTP API, and the settings screen all render and
// validate it the same way instead of each re-inferring it from the key.
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

// Setting groups, in registry order. The settings screen renders one heading
// per group, and harness config list prints the same grouping.
const (
	GroupCredentials     = "Credentials"
	GroupRunBudget       = "Run budget"
	GroupToolLimits      = "Tool limits"
	GroupModels          = "Models"
	GroupRequiresRestart = "Requires a restart"
)

// Setting keys. The registry below is the single source of truth; these
// constants exist so Go call sites name a key without string literals.
const (
	KeyDeepSeekAPIKey    = "deepseek.api_key"
	KeyGoogleAPIKey      = "google.api_key"
	KeyGoogleVisionModel = "google.vision_model"

	KeyRunMaxTokens           = "run.max_tokens"
	KeyRunMaxSubTurns         = "run.max_sub_turns"
	KeyRunDeadline            = "run.deadline"
	KeyRunCompactionThreshold = "run.compaction_threshold"

	KeyToolOutputCap                 = "tools.output_cap"
	KeyToolBashTimeout               = "tools.bash_timeout"
	KeyToolBashTimeoutMax            = "tools.bash_timeout_max"
	KeyToolTimeout                   = "tools.tool_timeout"
	KeyToolWebFetchTimeout           = "tools.webfetch_timeout"
	KeyToolTaskTimeout               = "tools.task_timeout"
	KeyToolReviewScreenshotTimeout   = "tools.reviewscreenshot_timeout"
	KeyToolWebFetchMaxBody           = "tools.webfetch_max_body"
	KeyToolWebFetchMaxExtract        = "tools.webfetch_max_extract"
	KeyToolReviewScreenshotMaxImages = "tools.reviewscreenshot_max_images"
	KeyToolReviewScreenshotMaxBytes  = "tools.reviewscreenshot_max_bytes"

	KeyDefaultModel      = "model.default"
	KeyDefaultFlashModel = "model.flash"
	KeyDefaultEffort     = "model.effort"

	KeyWorkerPoolSize         = "worker.pool_size"
	KeyWorkerConcurrencyPro   = "worker.model_concurrency_pro"
	KeyWorkerConcurrencyFlash = "worker.model_concurrency_flash"
	KeyQueueResultsMaxAge     = "queue.results_max_age"
	KeyHTTPEventsLimitDefault = "http.events_limit_default"
	KeyHTTPEventsLimitMax     = "http.events_limit_max"
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
	// Secret settings are masked by harness config list and GET /api/settings
	// (the last four characters show) and typed into a password field by the
	// screen.
	Secret bool
	// Restart marks a setting read once at startup or baked into a JetStream
	// stream: a change takes effect on the next start, not the next request.
	// The CLI, the API payload, and the screen all surface it.
	Restart bool
}

// registry is the ordered list of every setting, in the order harness config
// list prints and the screen renders. Groups run together in order:
// Credentials, Run budget, Tool limits, Models, Requires a restart.
var registry = []Descriptor{
	// --- Credentials ---
	stringSetting(KeyDeepSeekAPIKey, GroupCredentials,
		"DeepSeek API key — the harness's own account", "", true, false),
	stringSetting(KeyGoogleAPIKey, GroupCredentials,
		"Google API key — sent to Gemini by ReviewScreenshot", "", true, false),

	// --- Run budget ---
	intSetting(KeyRunMaxTokens, GroupRunBudget, 48000, 1, 1_000_000,
		"Default max output tokens for a run that omits max_tokens. The real ceiling is DeepSeek's output limit; this is the harness's own default."),
	intSetting(KeyRunMaxSubTurns, GroupRunBudget, 400, 1, 1_000_000,
		"Sub-turn budget a work request that omits max_sub_turns gets. Chosen against run.deadline: a flash sub-turn averages about six seconds, so a full 400-sub-turn run needs roughly 40 minutes of wall clock. Raising one without the other does nothing."),
	durationSetting(KeyRunDeadline, GroupRunBudget, "1h", time.Second, 365*24*time.Hour,
		"Wall clock a work request that omits deadline_ms gets. Chosen against run.max_sub_turns: 400 sub-turns at roughly six seconds each need about 40 minutes, and this hour leaves headroom. Raising one without the other does nothing."),
	intSetting(KeyRunCompactionThreshold, GroupRunBudget, 768*1024, 1024, 1_000_000,
		"Prompt-token threshold at which the session compacts its history (DeepSeek's recommended Claude Code compaction window, 768K of the 1M context)"),

	// --- Tool limits ---
	intSetting(KeyToolOutputCap, GroupToolLimits, 200_000, 1000, 10_000_000,
		"Output byte cap for every tool result, with truncation labelled in the result"),
	durationSetting(KeyToolBashTimeout, GroupToolLimits, "2m", time.Second, 24*time.Hour,
		"Default wall-clock timeout for a Bash call that omits timeout"),
	durationSetting(KeyToolBashTimeoutMax, GroupToolLimits, "10m", time.Second, 24*time.Hour,
		"Ceiling a Bash call's requested timeout is clamped to"),
	durationSetting(KeyToolTimeout, GroupToolLimits, "30s", time.Second, 24*time.Hour,
		"Default wall-clock timeout for every other tool"),
	durationSetting(KeyToolWebFetchTimeout, GroupToolLimits, "45s", time.Second, 24*time.Hour,
		"Wall-clock timeout for one WebFetch call"),
	durationSetting(KeyToolTaskTimeout, GroupToolLimits, "10m", time.Second, 24*time.Hour,
		"Wall-clock timeout for one Task subagent call"),
	durationSetting(KeyToolReviewScreenshotTimeout, GroupToolLimits, "60s", time.Second, 24*time.Hour,
		"Wall-clock timeout for one ReviewScreenshot Gemini call (generating a diagnosis routinely takes longer than the 30-second tool default)"),
	intSetting(KeyToolWebFetchMaxBody, GroupToolLimits, 4<<20, 1024, 1<<30,
		"Bytes of a fetched page read before extraction"),
	intSetting(KeyToolWebFetchMaxExtract, GroupToolLimits, 40_000, 100, 10_000_000,
		"Bytes of extracted text sent to the flash summarising call"),
	intSetting(KeyToolReviewScreenshotMaxImages, GroupToolLimits, 4, 1, 100,
		"Maximum screenshots one ReviewScreenshot call accepts"),
	intSetting(KeyToolReviewScreenshotMaxBytes, GroupToolLimits, 5<<20, 1024, 1<<30,
		"Maximum bytes per screenshot file (5 MB at the default)"),

	// --- Models ---
	stringSetting(KeyDefaultModel, GroupModels,
		"Default model for runs that omit model", "deepseek-v4-pro", false, false),
	stringSetting(KeyDefaultFlashModel, GroupModels,
		"Model Task subagents and WebFetch summarisation run", "deepseek-v4-flash", false, false),
	stringSetting(KeyDefaultEffort, GroupModels,
		"Default reasoning effort for runs that omit effort", "high", false, false).
		withAllowed("low", "high", "max"),
	stringSetting(KeyGoogleVisionModel, GroupModels,
		"Gemini model ReviewScreenshot sends screenshots to", "gemini-3.5-flash", false, false),

	// --- Requires a restart ---
	intSetting(KeyWorkerPoolSize, GroupRequiresRestart, 4, 1, 100_000,
		"Worker pool size, also the WORK consumer's MaxAckPending").withRestart(),
	intSetting(KeyWorkerConcurrencyPro, GroupRequiresRestart, 500, 1, 1_000_000,
		"Account-wide concurrent-request ceiling for the pro model").withRestart(),
	intSetting(KeyWorkerConcurrencyFlash, GroupRequiresRestart, 2500, 1, 1_000_000,
		"Account-wide concurrent-request ceiling for the flash model").withRestart(),
	durationSetting(KeyQueueResultsMaxAge, GroupRequiresRestart, "168h", time.Hour, 365*24*time.Hour,
		"RESULTS stream retention window").withRestart(),
	intSetting(KeyHTTPEventsLimitDefault, GroupRequiresRestart, 500, 1, 1_000_000,
		"Default page size for GET /api/sessions/{id}/events").withRestart(),
	intSetting(KeyHTTPEventsLimitMax, GroupRequiresRestart, 5000, 1, 1_000_000,
		"Largest page size GET /api/sessions/{id}/events accepts").withRestart(),
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

// ValidKeys lists every known setting key in registry order, the order
// harness config list prints and GET /api/settings serves. It is derived
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

// IsSecretKey reports whether key holds a credential that harness config and
// GET /api/settings mask by default. Derived from the registry.
func IsSecretKey(key string) bool {
	d, ok := Lookup(key)
	return ok && d.Secret
}

// validate rejects value unless it fits the descriptor's type and bounds.
// This is the one place a setting value is checked: harness config set, the
// HTTP PUT, and the screen's save all land here, so a rejected value reads
// identically from every surface.
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
