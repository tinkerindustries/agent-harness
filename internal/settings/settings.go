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
	// KeyGoogleAPIKey is the Google API key phase 2's screenshot-review
	// tool will read. Only the name is defined here; the client and the
	// tool are a later phase's work.
	KeyGoogleAPIKey = "google.api_key"
)

// ValidKeys lists every known setting key, in the order harness config list
// prints them.
var ValidKeys = []string{KeyDeepSeekAPIKey, KeyGoogleAPIKey}

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

func validate(key string) error {
	for _, k := range ValidKeys {
		if k == key {
			return nil
		}
	}
	return UnknownKeyError{Key: key}
}
