package settings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStore is the settings surface of *store.Store, backed by a map so the
// resolver's behaviour is testable without a database (TESTING.md: small
// interfaces satisfied by a struct declared in the test file).
type fakeStore struct {
	values map[string]string
}

func (f *fakeStore) Setting(ctx context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeStore) SetSetting(ctx context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakeStore) DeleteSetting(ctx context.Context, key string) error {
	delete(f.values, key)
	return nil
}

func TestResolverRoundTrip(t *testing.T) {
	r := NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()

	if _, ok, err := r.Get(ctx, KeyDeepSeekAPIKey); err != nil || ok {
		t.Fatalf("Get on empty store: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := r.Set(ctx, KeyDeepSeekAPIKey, "sk-abc"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, ok, err := r.Get(ctx, KeyDeepSeekAPIKey)
	if err != nil || !ok || value != "sk-abc" {
		t.Fatalf("Get = %q ok=%v err=%v, want sk-abc true nil", value, ok, err)
	}

	got, err := r.DeepSeekAPIKey(ctx)
	if err != nil {
		t.Fatalf("DeepSeekAPIKey: %v", err)
	}
	if got != "sk-abc" {
		t.Fatalf("DeepSeekAPIKey = %q, want sk-abc", got)
	}

	if err := r.Unset(ctx, KeyDeepSeekAPIKey); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	got, err = r.DeepSeekAPIKey(ctx)
	if err != nil || got != "" {
		t.Fatalf("DeepSeekAPIKey after unset = %q err=%v, want \"\" nil", got, err)
	}
}

// TestResolverReadsThroughOnEveryCall pins that the resolver caches nothing:
// a key written behind its back — the same database, another process — is
// visible on the very next call, which is what lets an operator set a key
// while harness serve is running and have the next request pick it up.
func TestResolverReadsThroughOnEveryCall(t *testing.T) {
	st := &fakeStore{values: map[string]string{}}
	r := NewResolver(st)
	ctx := context.Background()

	if _, ok, _ := r.Get(ctx, KeyDeepSeekAPIKey); ok {
		t.Fatal("expected the key to start unset")
	}
	st.values[KeyDeepSeekAPIKey] = "sk-new"
	value, ok, err := r.Get(ctx, KeyDeepSeekAPIKey)
	if err != nil || !ok || value != "sk-new" {
		t.Fatalf("Get after external change = %q ok=%v err=%v, want sk-new true nil", value, ok, err)
	}
}

func TestResolverRejectsUnknownKeys(t *testing.T) {
	r := NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()

	for _, fn := range []func() error{
		func() error { _, _, err := r.Get(ctx, "deepsek.api_key"); return err },
		func() error { return r.Set(ctx, "deepsek.api_key", "sk-...") },
		func() error { return r.Unset(ctx, "deepsek.api_key") },
	} {
		err := fn()
		var ue UnknownKeyError
		if !errors.As(err, &ue) {
			t.Fatalf("error = %v, want UnknownKeyError", err)
		}
		msg := ue.Error()
		for _, want := range []string{`unknown setting "deepsek.api_key"`, "valid settings: deepseek.api_key, kimi.api_key, google.api_key", "worker.pool_size"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error text %q does not contain %q", msg, want)
			}
		}
	}
}

// TestGoogleKeysPins the two phase-2 resolver methods: GoogleAPIKey reads
// the stored key ("" when unset) and GoogleVisionModel defaults to
// gemini-3.5-flash when unset and honours a stored value.
func TestGoogleKeys(t *testing.T) {
	r := NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()

	key, err := r.GoogleAPIKey(ctx)
	if err != nil || key != "" {
		t.Fatalf("GoogleAPIKey on empty store = %q err=%v, want \"\" nil", key, err)
	}

	model, err := r.GoogleVisionModel(ctx)
	if err != nil || model != "gemini-3.5-flash" {
		t.Fatalf("GoogleVisionModel on empty store = %q err=%v, want gemini-3.5-flash nil", model, err)
	}

	if err := r.Set(ctx, KeyGoogleAPIKey, "gk-abc"); err != nil {
		t.Fatalf("Set google.api_key: %v", err)
	}
	if err := r.Set(ctx, KeyGoogleVisionModel, "gemini-3.6-flash"); err != nil {
		t.Fatalf("Set google.vision_model: %v", err)
	}
	key, err = r.GoogleAPIKey(ctx)
	if err != nil || key != "gk-abc" {
		t.Fatalf("GoogleAPIKey = %q err=%v, want gk-abc nil", key, err)
	}
	model, err = r.GoogleVisionModel(ctx)
	if err != nil || model != "gemini-3.6-flash" {
		t.Fatalf("GoogleVisionModel = %q err=%v, want gemini-3.6-flash nil", model, err)
	}
}

func TestIsSecretKey(t *testing.T) {
	if !IsSecretKey(KeyDeepSeekAPIKey) {
		t.Error("deepseek.api_key must be treated as a secret")
	}
	if !IsSecretKey(KeyKimiAPIKey) {
		t.Error("kimi.api_key must be treated as a secret")
	}
	if !IsSecretKey(KeyGoogleAPIKey) {
		t.Error("google.api_key must be treated as a secret")
	}
	if IsSecretKey(KeyGoogleVisionModel) {
		t.Error("google.vision_model is not a secret and must print in full")
	}
	for _, key := range []string{KeyRunMaxTokens, KeyWorkerPoolSize, KeyDefaultModel, KeyToolOutputCap} {
		if IsSecretKey(key) {
			t.Errorf("%s is not a credential and must not be masked", key)
		}
	}
}

// TestKimiAPIKey pins the phase-5 resolver method: KimiAPIKey reads the
// stored key and returns "" when unset — the exact shape the kimi client's
// per-request key provider needs.
func TestKimiAPIKey(t *testing.T) {
	r := NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()

	key, err := r.KimiAPIKey(ctx)
	if err != nil || key != "" {
		t.Fatalf("KimiAPIKey on empty store = %q err=%v, want \"\" nil", key, err)
	}

	if err := r.Set(ctx, KeyKimiAPIKey, "sk-kimi-abc"); err != nil {
		t.Fatalf("Set kimi.api_key: %v", err)
	}
	key, err = r.KimiAPIKey(ctx)
	if err != nil || key != "sk-kimi-abc" {
		t.Fatalf("KimiAPIKey = %q err=%v, want sk-kimi-abc nil", key, err)
	}
}

// TestTypedAccessorsResolveDefaultsAndStoredValues pins the registry's
// contract: with nothing stored, String/Int/Duration return the registry
// default — the exact constants the limits used to be — and a stored value
// wins.
func TestTypedAccessorsResolveDefaultsAndStoredValues(t *testing.T) {
	r := NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()

	if v, err := r.Int(ctx, KeyRunMaxTokens); err != nil || v != 48000 {
		t.Fatalf("Int(run.max_tokens) on empty store = %d err=%v, want 48000 nil", v, err)
	}
	if v, err := r.Duration(ctx, KeyRunDeadline); err != nil || v != 60*time.Minute {
		t.Fatalf("Duration(run.deadline) on empty store = %v err=%v, want 1h nil", v, err)
	}
	if v, err := r.String(ctx, KeyDefaultModel); err != nil || v != "deepseek-v4-pro" {
		t.Fatalf("String(model.default) on empty store = %q err=%v, want deepseek-v4-pro nil", v, err)
	}

	if err := r.Set(ctx, KeyRunMaxTokens, "100"); err != nil {
		t.Fatalf("Set run.max_tokens: %v", err)
	}
	if v, err := r.Int(ctx, KeyRunMaxTokens); err != nil || v != 100 {
		t.Fatalf("Int(run.max_tokens) after Set = %d err=%v, want 100 nil", v, err)
	}
}

// TestSetRejectsValuesOutOfBounds pins that validation lives in the registry
// and fires on every write: a negative bash timeout, an effort outside the
// enum, a non-integer max_tokens, and an out-of-range page limit all fail
// with a ValidationError carrying the descriptor's bounds.
func TestSetRejectsValuesOutOfBounds(t *testing.T) {
	r := NewResolver(&fakeStore{values: map[string]string{}})
	ctx := context.Background()

	cases := []struct {
		key   string
		value string
		want  string
	}{
		{KeyToolBashTimeout, "-5s", "out of range"},
		{KeyToolBashTimeout, "not a duration", "is not a duration"},
		{KeyRunMaxTokens, "not a number", "is not an integer"},
		{KeyRunMaxTokens, "0", "out of range"},
		{KeyRunMaxTokens, "5000000000", "out of range"},
		{KeyDefaultEffort, "turbo", "must be one of low, high, max"},
		{KeyHTTPEventsLimitMax, "0", "out of range"},
	}
	for _, tc := range cases {
		err := r.Set(ctx, tc.key, tc.value)
		var ve ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("Set(%s, %q) error = %v, want ValidationError", tc.key, tc.value, err)
		}
		if !strings.Contains(ve.Error(), tc.want) {
			t.Errorf("Set(%s, %q) error %q does not contain %q", tc.key, tc.value, ve.Error(), tc.want)
		}
		if _, ok, err := r.Get(ctx, tc.key); err != nil || ok {
			t.Errorf("Set(%s, %q) stored the rejected value (ok=%v err=%v)", tc.key, tc.value, ok, err)
		}
	}
}

// TestRestartFlagsPins the six settings that need a restart, so the CLI and
// the screen keep marking exactly them.
func TestRestartFlags(t *testing.T) {
	for _, key := range []string{
		KeyWorkerPoolSize, KeyWorkerConcurrencyPro, KeyWorkerConcurrencyFlash,
		KeyQueueResultsMaxAge, KeyHTTPEventsLimitDefault, KeyHTTPEventsLimitMax,
	} {
		d, ok := Lookup(key)
		if !ok || !d.Restart {
			t.Errorf("%s must carry the restart flag", key)
		}
	}
	for _, key := range []string{KeyRunMaxTokens, KeyToolOutputCap, KeyDefaultModel, KeyGoogleVisionModel} {
		if d, ok := Lookup(key); !ok || d.Restart {
			t.Errorf("%s must not carry the restart flag", key)
		}
	}
}
