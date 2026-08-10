package settings

import (
	"context"
	"errors"
	"testing"
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
		want := `unknown setting "deepsek.api_key"; valid settings: deepseek.api_key, google.api_key, google.vision_model`
		if ue.Error() != want {
			t.Fatalf("error text = %q, want %q", ue.Error(), want)
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
	if err != nil || model != DefaultGoogleVisionModel {
		t.Fatalf("GoogleVisionModel on empty store = %q err=%v, want %q nil", model, err, DefaultGoogleVisionModel)
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
	if !IsSecretKey(KeyGoogleAPIKey) {
		t.Error("google.api_key must be treated as a secret")
	}
	if IsSecretKey(KeyGoogleVisionModel) {
		t.Error("google.vision_model is not a secret and must print in full")
	}
}
