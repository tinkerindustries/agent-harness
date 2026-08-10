// Package settings names the harness's stored settings and resolves them
// through the SQLite store. Unlike the rest of configuration, which arrives
// as environment variables read once at startup, settings live in the
// database so an operator can change a key while harness serve is running
// and have the next request pick the change up without a restart.
package settings

import (
	"context"
	"fmt"
	"strings"
)

const (
	// KeyDeepSeekAPIKey is the DeepSeek API key, stored in plaintext by the
	// operator's deliberate choice.
	KeyDeepSeekAPIKey = "deepseek.api_key"
	// KeyGoogleAPIKey is the Google API key the ReviewScreenshot tool sends
	// to Gemini. Phase 2's client reads it on every call via
	// Resolver.GoogleAPIKey.
	KeyGoogleAPIKey = "google.api_key"
	// KeyGoogleVisionModel is the Gemini model ReviewScreenshot sends
	// screenshots to, so the model can be changed without a rebuild. It is
	// not a secret, so harness config list prints it in full; it defaults to
	// gemini.DefaultModel when unset (Resolver.GoogleVisionModel).
	KeyGoogleVisionModel = "google.vision_model"
)

// DefaultGoogleVisionModel is the model ReviewScreenshot uses when
// google.vision_model is unset. It must match gemini.DefaultModel.
const DefaultGoogleVisionModel = "gemini-3.5-flash"

// ValidKeys lists every known setting key, in the order harness config list
// prints them.
var ValidKeys = []string{KeyDeepSeekAPIKey, KeyGoogleAPIKey, KeyGoogleVisionModel}

// SecretKeys lists the setting keys whose values are credentials and must be
// masked by harness config list/get unless -reveal is given. Everything else
// — model names and the like — prints in full.
var SecretKeys = []string{KeyDeepSeekAPIKey, KeyGoogleAPIKey}

// IsSecretKey reports whether key holds a credential that harness config
// masks by default.
func IsSecretKey(key string) bool {
	for _, k := range SecretKeys {
		if k == key {
			return true
		}
	}
	return false
}

// UnknownKeyError reports a read or write of a key outside the known set, so
// a typo like "deepsek.api_key" fails loudly instead of silently storing a
// setting nothing ever reads.
type UnknownKeyError struct{ Key string }

func (e UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown setting %q; valid settings: %s", e.Key, strings.Join(ValidKeys, ", "))
}

// Store is the settings surface of *store.Store, declared as an interface so
// this package depends on the three methods it uses, not the whole store.
type Store interface {
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
	DeleteSetting(ctx context.Context, key string) error
}

// Resolver reads and writes settings through to the store on every call and
// caches nothing, so a key changed by another process takes effect on the
// next request without restarting anything.
type Resolver struct {
	store Store
}

// NewResolver returns a Resolver reading and writing st.
func NewResolver(st Store) *Resolver {
	return &Resolver{store: st}
}

// Get returns key's value, or ok=false when it is unset.
func (r *Resolver) Get(ctx context.Context, key string) (string, bool, error) {
	if err := validate(key); err != nil {
		return "", false, err
	}
	return r.store.Setting(ctx, key)
}

// Set writes key, rejecting any key outside the known set with an error that
// lists the valid ones.
func (r *Resolver) Set(ctx context.Context, key, value string) error {
	if err := validate(key); err != nil {
		return err
	}
	return r.store.SetSetting(ctx, key, value)
}

// Unset deletes key. Deleting an unset key is not an error.
func (r *Resolver) Unset(ctx context.Context, key string) error {
	if err := validate(key); err != nil {
		return err
	}
	return r.store.DeleteSetting(ctx, key)
}

// DeepSeekAPIKey returns the stored DeepSeek API key, "" when unset — the
// exact shape the deepseek client's per-request key provider needs.
func (r *Resolver) DeepSeekAPIKey(ctx context.Context) (string, error) {
	v, _, err := r.store.Setting(ctx, KeyDeepSeekAPIKey)
	return v, err
}

// GoogleAPIKey returns the stored Google API key, "" when unset — the exact
// shape the gemini client's per-request key provider needs.
func (r *Resolver) GoogleAPIKey(ctx context.Context) (string, error) {
	v, _, err := r.store.Setting(ctx, KeyGoogleAPIKey)
	return v, err
}

// GoogleVisionModel returns the stored Gemini vision model, DefaultGoogleVisionModel
// when unset — the exact shape the ReviewScreenshot tool's per-call model
// provider needs.
func (r *Resolver) GoogleVisionModel(ctx context.Context) (string, error) {
	v, ok, err := r.store.Setting(ctx, KeyGoogleVisionModel)
	if err != nil || ok {
		return v, err
	}
	return DefaultGoogleVisionModel, nil
}

func validate(key string) error {
	for _, k := range ValidKeys {
		if k == key {
			return nil
		}
	}
	return UnknownKeyError{Key: key}
}
