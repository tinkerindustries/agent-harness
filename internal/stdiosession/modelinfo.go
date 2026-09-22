package stdiosession

import (
	"slices"

	"github.com/mrgeoffrich/agent-harness/internal/anthropic"
	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
)

// This file is the one place the protocol's model vocabulary meets a
// provider's. Everything else under internal/stdiosession is provider-neutral:
// the frames are Google's, the loop underneath is internal/session, and which
// company serves a model is a fact only these four functions consult.
//
// The protocol says `reasoning.effort` because that is what the Responses
// API calls it, and Gemini spells the same idea `thinking_level` with its
// own set of values. The translation is one hop — the effort travels as
// wire.ChatIntent.Effort and each provider spells it
// (internal/deepseek/intent.go, internal/gemini/intent.go) — so what is
// needed here is only the answer to "which values may this model be sent",
// per model, for the handshake to publish and the create to check.

// displayName is a human-readable name for model, or "" when no provider
// offers one; a client falls back to the id.
func displayName(model string) string {
	switch providerOf(model) {
	case provider.DeepSeek:
		return deepseek.DisplayName(model)
	case provider.Anthropic:
		return anthropic.DisplayName(model)
	default:
		return gemini.DisplayName(model)
	}
}

// contextWindowTokens is model's total input token budget, or zero when no
// provider has a figure for it — which ModelDetail then omits rather than
// guesses at.
func contextWindowTokens(model string) int {
	switch providerOf(model) {
	case provider.DeepSeek:
		return deepseek.ContextWindowTokens(model)
	case provider.Anthropic:
		return anthropic.ContextWindowTokens(model)
	default:
		return gemini.ContextWindowTokens(model)
	}
}

// reasoningEfforts is what reasoning.effort may be for model.
// Empty means this process has no table for it and any level reaches the API
// for it to judge.
//
// A Gemini model's set includes "medium" and may include "minimal", neither
// of which DeepSeek advertises. That is deliberate and is recorded in
// docs/STDIO-PROTOCOL.md: the handshake's job is to say what this model
// takes, and a client that reads model_details — which the protocol already
// tells it to, because the Gemini models disagree with each other about
// "minimal" — gets it right without knowing whose model it is.
func reasoningEfforts(model string) []string {
	switch providerOf(model) {
	case provider.DeepSeek:
		return deepseek.EffortsFor(model)
	case provider.Anthropic:
		return anthropic.EffortLevelsFor(model)
	default:
		return gemini.LevelsFor(model)
	}
}

// reasoningEffortSupported reports whether model accepts effort. A model no
// table knows accepts anything.
//
// DeepSeek accepts two spellings it does not advertise ("medium" and
// "xhigh", both mapped onto "high"), so this is not simply membership of
// thinkingLevels — which is why the check is a function of the provider's
// rather than a slices.Contains here.
func reasoningEffortSupported(model, effort string) bool {
	switch providerOf(model) {
	case provider.DeepSeek:
		return deepseek.EffortSupported(model, effort)
	case provider.Anthropic:
		return anthropic.EffortSupported(model, effort)
	default:
		return gemini.LevelSupported(model, effort)
	}
}

// missingKeyMessage names the environment variable whose absence stopped a
// create on model, in the message the -32003 error carries. The parent
// supplies credentials (docs/STDIO-PROTOCOL.md, "Starting the process"), and
// which one it forgot depends on which model it asked for, so the error says
// so rather than naming Google's variables for a DeepSeek run.
func missingKeyMessage(model string) string {
	switch providerOf(model) {
	case provider.DeepSeek:
		return "no DeepSeek API key reached this process: set DEEPSEEK_API_KEY in the environment you spawn it with"
	case provider.Anthropic:
		return "no Anthropic API key reached this process: set ANTHROPIC_API_KEY in the environment you spawn it with"
	default:
		return "no Google API key reached this process: set GEMINI_API_KEY (or GOOGLE_API_KEY) in the environment you spawn it with"
	}
}

// providerOf is provider.ModelFor with its error folded into the default
// branch: a model no table routes is treated as Gemini's, which is what
// every function here did before DeepSeek was hosted and keeps an unknown
// name reaching the same fallbacks rather than a second failure mode.
func providerOf(model string) provider.Name {
	p, err := provider.ModelFor(model)
	if err != nil {
		return provider.Gemini
	}
	return p
}

// accepts reports whether model is one this process advertised. It is
// membership of the handshake's own list rather than provider.Known: this
// binary hosts a chosen few of the models the repository can route — one
// DeepSeek model, the vision-capable one — and a create naming any other is
// refused with the accepted set named (docs/STDIO-PROTOCOL.md, "models").
func accepts(models []string, model string) bool {
	return slices.Contains(models, model)
}
